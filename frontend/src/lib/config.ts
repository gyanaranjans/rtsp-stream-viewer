// Backend base URL. Empty means same origin (dev proxy or backend serving the build).
const backend = (import.meta.env.VITE_BACKEND_URL as string | undefined)?.replace(/\/$/, '') ?? ''

export function wsUrlFor(rtspUrl: string) {
  const base = backend || window.location.origin
  return `${base.replace(/^http/, 'ws')}/ws?url=${encodeURIComponent(rtspUrl)}`
}

// A public camera plus test streams published by the bundled MediaMTX next to the backend.
export const DEMO_STREAMS: string[] = (
  (import.meta.env.VITE_DEMO_STREAMS as string | undefined) ??
  'rtsp://stream.strba.sk:1935/strba/VYHLAD_JAZERO.stream,rtsp://localhost:8554/testsrc,rtsp://localhost:8554/smpte,rtsp://localhost:8554/life,rtsp://localhost:8554/hevc'
)
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean)

export function validateRtspUrl(raw: string): string | null {
  let u: URL
  try {
    u = new URL(raw)
  } catch {
    return 'Enter a valid URL, e.g. rtsp://host:554/stream'
  }
  if (u.protocol !== 'rtsp:' && u.protocol !== 'rtsps:') return 'URL must start with rtsp:// or rtsps://'
  if (!u.hostname) return 'URL is missing a host'
  return null
}

export function displayName(raw: string) {
  try {
    const u = new URL(raw)
    return `${u.hostname}${u.port ? ':' + u.port : ''}${u.pathname}`
  } catch {
    return raw
  }
}
