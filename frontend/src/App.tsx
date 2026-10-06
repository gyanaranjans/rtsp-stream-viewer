import { useEffect, useState, type FormEvent } from 'react'
import { StreamTile, type Stream } from './components/StreamTile'
import { DEMO_STREAMS, validateRtspUrl } from './lib/config'
import { isSupported } from './lib/player'

const STORAGE_KEY = 'rtsp-viewer:streams'
const LAYOUTS = ['auto', '1', '2', '3', '4'] as const
type Layout = (typeof LAYOUTS)[number]

function loadStreams(): Stream[] {
  try {
    return JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '[]')
  } catch {
    return []
  }
}

export default function App() {
  const [streams, setStreams] = useState<Stream[]>(loadStreams)
  const [input, setInput] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [layout, setLayout] = useState<Layout>('auto')

  useEffect(() => {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(streams))
  }, [streams])

  const add = (urls: string[]) =>
    setStreams((prev) => [
      ...prev,
      ...urls.filter((u) => !prev.some((s) => s.url === u)).map((url) => ({ id: crypto.randomUUID(), url })),
    ])

  const submit = (e: FormEvent) => {
    e.preventDefault()
    const url = input.trim()
    const err = validateRtspUrl(url)
    if (err) return setError(err)
    if (streams.some((s) => s.url === url)) return setError('That stream is already in the grid')
    add([url])
    setInput('')
    setError(null)
  }

  const gridStyle =
    layout === 'auto' ? undefined : { gridTemplateColumns: `repeat(${layout}, minmax(0, 1fr))` }

  return (
    <div className="app">
      <header className="header">
        <div className="brand">
          <span className="logo" aria-hidden>◉</span>
          <h1>RTSP Stream Viewer</h1>
        </div>
        <form className="add-form" onSubmit={submit} noValidate>
          <input
            type="url"
            value={input}
            onChange={(e) => {
              setInput(e.target.value)
              setError(null)
            }}
            placeholder="rtsp://user:pass@camera.local:554/stream"
            aria-label="RTSP stream URL"
            aria-invalid={!!error}
            spellCheck={false}
          />
          <button type="submit">Add stream</button>
        </form>
        {error && <p className="form-error" role="alert">{error}</p>}
      </header>

      {!isSupported() && (
        <p className="banner">This browser lacks Media Source Extensions, so streams cannot play.</p>
      )}

      {streams.length > 0 && (
        <div className="toolbar">
          <span>{streams.length} stream{streams.length === 1 ? '' : 's'}</span>
          <div className="layout-picker" role="group" aria-label="Grid columns">
            {LAYOUTS.map((l) => (
              <button key={l} className={l === layout ? 'active' : ''} onClick={() => setLayout(l)}>
                {l === 'auto' ? 'Auto' : `${l} col`}
              </button>
            ))}
          </div>
          <button className="link" onClick={() => setStreams([])}>Remove all</button>
        </div>
      )}

      {streams.length === 0 ? (
        <section className="empty">
          <h2>No streams yet</h2>
          <p>Paste an RTSP URL above, or load the demo streams served by the bundled MediaMTX server.</p>
          <button onClick={() => add(DEMO_STREAMS)}>Load demo streams</button>
        </section>
      ) : (
        <main className="grid" style={gridStyle}>
          {streams.map((s) => (
            <StreamTile key={s.id} stream={s} onRemove={() => setStreams((p) => p.filter((x) => x.id !== s.id))} />
          ))}
        </main>
      )}
    </div>
  )
}
