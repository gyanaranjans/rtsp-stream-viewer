// Plays the backend's WebSocket fMP4 feed through Media Source Extensions.
//
// Protocol (server -> client):
//   text   {"type":"status","state":"connecting|live|reconnecting","message"?}
//   text   {"type":"init","codec":"avc1.xxxxxx"}  followed by binary init segment
//   text   {"type":"error","message"}             server is closing this socket
//   binary moof+mdat fragment (each one starts on a keyframe)

export type PlayerState = 'idle' | 'connecting' | 'live' | 'reconnecting' | 'error' | 'paused'

export interface PlayerStatus {
  state: PlayerState
  message?: string
}

type ServerEvent =
  | { type: 'status'; state: 'connecting' | 'live' | 'reconnecting'; message?: string }
  | { type: 'init'; codec: string }
  | { type: 'error'; message: string }

// Keep latency low: if we drift this far behind the live edge, jump forward.
const MAX_LATENCY_SEC = 2.5
const BUFFER_KEEP_SEC = 10
const MAX_RETRY_DELAY_MS = 15_000

const MediaSourceImpl: (typeof MediaSource) | undefined =
  // ManagedMediaSource is the only MSE available on iOS Safari.
  (globalThis as unknown as { ManagedMediaSource?: typeof MediaSource }).ManagedMediaSource ??
  globalThis.MediaSource

export function isSupported() {
  return MediaSourceImpl !== undefined
}

export class StreamPlayer {
  private ws?: WebSocket
  private ms?: MediaSource
  private sb?: SourceBuffer
  private objectUrl?: string
  private queue: ArrayBuffer[] = []
  private stopped = true
  private retries = 0
  private retryTimer?: number
  private readonly video: HTMLVideoElement
  private readonly wsUrl: string
  private readonly emit: (s: PlayerStatus) => void
  private state: PlayerState = 'idle'

  constructor(video: HTMLVideoElement, wsUrl: string, onStatus: (s: PlayerStatus) => void) {
    this.video = video
    this.wsUrl = wsUrl
    this.emit = onStatus
  }

  private onTimeUpdate = () => {
    if (!this.stopped && this.sb && this.state !== 'live' && !this.video.paused) this.onStatus({ state: 'live' })
  }

  private onStatus(s: PlayerStatus) {
    this.state = s.state
    this.emit(s)
  }

  start() {
    if (!isSupported()) {
      this.onStatus({ state: 'error', message: 'This browser does not support Media Source Extensions' })
      return
    }
    this.stopped = false
    this.video.addEventListener('timeupdate', this.onTimeUpdate)
    this.connect()
  }

  /** Pause disconnects entirely so a paused tile costs no bandwidth or server CPU. */
  stop(state: PlayerState = 'paused') {
    this.stopped = true
    this.video.removeEventListener('timeupdate', this.onTimeUpdate)
    window.clearTimeout(this.retryTimer)
    this.closeSocket()
    this.resetMedia()
    this.onStatus({ state })
  }

  private connect() {
    this.closeSocket()
    this.onStatus({ state: this.retries ? 'reconnecting' : 'connecting' })
    const ws = new WebSocket(this.wsUrl)
    ws.binaryType = 'arraybuffer'
    this.ws = ws
    let fatal: string | undefined

    ws.onmessage = (e) => {
      if (typeof e.data === 'string') {
        const ev = JSON.parse(e.data) as ServerEvent
        if (ev.type === 'init') {
          this.setupMedia(ev.codec)
        } else if (ev.type === 'status') {
          if (ev.state === 'live') this.retries = 0
          // 'live' is reported once frames are actually playing (see onUpdateEnd).
          if (ev.state !== 'live') this.onStatus({ state: ev.state, message: ev.message })
        } else if (ev.type === 'error') {
          fatal = ev.message
        }
        return
      }
      this.enqueue(e.data as ArrayBuffer)
    }

    ws.onclose = (e) => {
      if (this.ws !== ws || this.stopped) return
      this.ws = undefined
      this.scheduleReconnect(fatal ?? (e.reason || 'Connection to server lost'))
    }
  }

  private scheduleReconnect(message: string) {
    const delay = Math.min(1000 * 2 ** this.retries, MAX_RETRY_DELAY_MS)
    this.retries++
    this.onStatus({ state: 'reconnecting', message: `${message} — retrying in ${Math.round(delay / 1000)}s` })
    this.retryTimer = window.setTimeout(() => this.connect(), delay)
  }

  private closeSocket() {
    if (!this.ws) return
    const ws = this.ws
    this.ws = undefined
    ws.onmessage = ws.onclose = null
    ws.close()
  }

  private setupMedia(codec: string) {
    this.resetMedia()
    const mime = `video/mp4; codecs="${codec}"`
    if (!MediaSourceImpl!.isTypeSupported(mime)) {
      this.stop('error')
      this.onStatus({ state: 'error', message: `Codec ${codec} is not supported by this browser` })
      return
    }
    const ms = new MediaSourceImpl!()
    this.ms = ms
    // Required for ManagedMediaSource on Safari.
    this.video.disableRemotePlayback = true
    this.objectUrl = URL.createObjectURL(ms)
    this.video.src = this.objectUrl
    ms.addEventListener('sourceopen', () => {
      if (this.ms !== ms) return
      this.sb = ms.addSourceBuffer(mime)
      this.sb.mode = 'segments'
      this.sb.addEventListener('updateend', () => this.onUpdateEnd())
      this.flush()
    }, { once: true })
  }

  private resetMedia() {
    this.queue = []
    this.sb = undefined
    this.ms = undefined
    if (this.objectUrl) {
      URL.revokeObjectURL(this.objectUrl)
      this.objectUrl = undefined
      this.video.removeAttribute('src')
      this.video.load()
    }
  }

  private enqueue(buf: ArrayBuffer) {
    if (!this.ms) return // fragment arrived before an init segment; drop it
    // If decoding can't keep up, drop the backlog and resume at the newest fragment
    // (every fragment starts with a keyframe, so this is always safe).
    if (this.queue.length > 30) this.queue = []
    this.queue.push(buf)
    this.flush()
  }

  private flush() {
    const sb = this.sb
    if (!sb || sb.updating || this.queue.length === 0) return
    try {
      sb.appendBuffer(this.queue.shift()!)
    } catch (err) {
      if ((err as DOMException).name === 'QuotaExceededError') {
        this.trim(true)
        return
      }
      // Media pipeline broke (e.g. decode error); start over with a fresh socket.
      this.resetMedia()
      if (!this.stopped) this.scheduleReconnect('Playback error')
    }
  }

  private onUpdateEnd() {
    const sb = this.sb
    if (!sb) return
    const v = this.video
    if (sb.buffered.length) {
      const end = sb.buffered.end(sb.buffered.length - 1)
      const start = sb.buffered.start(sb.buffered.length - 1)
      if (v.currentTime < start || end - v.currentTime > MAX_LATENCY_SEC) {
        v.currentTime = Math.max(start, end - 0.3)
      }
      if (v.paused) v.play().catch(() => {})
    }
    if (!this.trim(false)) this.flush()
  }

  /** Removes old media from the SourceBuffer. Returns true if a remove was started. */
  private trim(aggressive: boolean) {
    const sb = this.sb
    if (!sb || sb.updating || !sb.buffered.length) return false
    const keep = aggressive ? 1 : BUFFER_KEEP_SEC
    const cut = this.video.currentTime - keep
    if (sb.buffered.start(0) < cut - 1) {
      sb.remove(0, cut)
      return true
    }
    return false
  }
}
