import { useEffect, useRef, useState } from 'react'
import { StreamPlayer, type PlayerStatus } from '../lib/player'
import { displayName, wsUrlFor } from '../lib/config'

export interface Stream {
  id: string
  url: string
}

const LABELS: Record<PlayerStatus['state'], string> = {
  idle: 'Idle',
  connecting: 'Connecting',
  live: 'Live',
  reconnecting: 'Reconnecting',
  error: 'Error',
  paused: 'Paused',
}

export function StreamTile({ stream, onRemove }: { stream: Stream; onRemove: () => void }) {
  const videoRef = useRef<HTMLVideoElement>(null)
  const tileRef = useRef<HTMLDivElement>(null)
  const playerRef = useRef<StreamPlayer | null>(null)
  const [status, setStatus] = useState<PlayerStatus>({ state: 'connecting' })
  const [playing, setPlaying] = useState(true)

  useEffect(() => {
    const video = videoRef.current!
    const player = new StreamPlayer(video, wsUrlFor(stream.url), setStatus)
    playerRef.current = player
    player.start()
    return () => player.stop()
  }, [stream.url])

  const toggle = () => {
    const player = playerRef.current!
    if (playing) player.stop('paused')
    else player.start()
    setPlaying(!playing)
  }

  const fullscreen = () => {
    if (document.fullscreenElement) document.exitFullscreen()
    else tileRef.current?.requestFullscreen()
  }

  const showOverlay = status.state !== 'live'

  return (
    <div className="tile" ref={tileRef} data-state={status.state}>
      <div className="video-wrap">
        <video ref={videoRef} muted playsInline autoPlay />
        {showOverlay && (
          <div className="overlay" role="status">
            {(status.state === 'connecting' || status.state === 'reconnecting') && <div className="spinner" />}
            {status.state === 'paused' && (
              <button className="big-play" onClick={toggle} aria-label="Resume stream">▶</button>
            )}
            <p className="overlay-title">{LABELS[status.state]}</p>
            {status.message && <p className="overlay-msg">{status.message}</p>}
          </div>
        )}
      </div>
      <div className="tile-bar">
        <span className={`badge badge-${status.state}`}>{LABELS[status.state]}</span>
        <span className="tile-name" title={stream.url}>{displayName(stream.url)}</span>
        <div className="tile-actions">
          <button onClick={toggle} aria-label={playing ? 'Pause stream' : 'Play stream'} title={playing ? 'Pause' : 'Play'}>
            {playing ? '❚❚' : '▶'}
          </button>
          <button onClick={fullscreen} aria-label="Toggle fullscreen" title="Fullscreen">⛶</button>
          <button onClick={onRemove} aria-label="Remove stream" title="Remove" className="danger">✕</button>
        </div>
      </div>
    </div>
  )
}
