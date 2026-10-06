# RTSP Stream Viewer

Add RTSP camera URLs in the browser and watch them live, side by side in a grid.

- **Frontend:** React 19, TypeScript, Vite. Video plays through Media Source Extensions.
- **Backend:** Go. FFmpeg turns each RTSP source into fragmented MP4, and the backend sends it to browsers over WebSockets.
- **Test streams:** MediaMTX, with FFmpeg generating synthetic sources (H.264 and H.265).

## Features

- Add `rtsp://` / `rtsps://` URLs. They are checked in the browser and on the server, and saved in `localStorage`.
- Grid layout: auto-fit, or a fixed 1–4 columns. On mobile it switches to a single column.
- Each tile has play/pause, fullscreen and remove buttons, plus a status badge: Connecting, Live, Reconnecting, Paused or Error.
- Errors are shown in plain language, for example "connection refused", "stream not found (404)", "authentication failed (401)" or "stream stalled".
- Both sides reconnect automatically with exponential backoff.
- Non-H.264 sources such as H.265 are detected and transcoded to H.264 automatically. H.264 sources are passed through with `-c copy`, which costs almost no CPU.

## Architecture

```
Browser tile ──WS /ws?url=rtsp://…──▶ Go backend ──▶ stream.Manager
   ▲  MSE SourceBuffer                                   │ one Stream per unique URL
   │                                                     ▼
   └──── init segment + moof/mdat fragments ◀── FFmpeg (rtsp → fMP4 on stdout)
```

- **One FFmpeg process per unique URL, shared by any number of viewers.** Ten browsers watching one camera still open only one RTSP session.
- FFmpeg runs with `-movflags frag_keyframe+empty_moov+default_base_moof`. The backend parses the MP4 boxes, caches `ftyp+moov` as the init segment, and sends each `moof+mdat` pair as one WebSocket binary message.
- Every fragment starts on a keyframe. A viewer who joins late gets the cached init segment and starts playing from the next fragment.
- WebSocket protocol:
  - Text messages are JSON control messages: `status`, `init` (which carries the codec string) and `error`.
  - Binary messages are media.
- **Backpressure.** Each subscriber has its own bounded queue. A client that can't keep up is disconnected rather than slowing down the other viewers; it then reconnects at the next keyframe. On the browser side:
  - The player trims old buffered video.
  - It jumps to the live edge if it falls more than 2.5 s behind.
- **Resource use:**
  - Pausing a tile closes its socket.
  - A stream with no viewers is stopped after `IDLE_TIMEOUT_SEC`.
  - `MAX_STREAMS` limits how many FFmpeg processes run at once.
- **Supervision.** If FFmpeg exits or stalls (no data for `STALL_TIMEOUT_SEC`), it is restarted with backoff from 1 s up to 30 s, and every viewer is told why.

```
backend/
  main.go                  HTTP server, /ws, /api/streams, /healthz, static hosting
  internal/stream/         FFmpeg supervisor, MP4 box parser, fan-out hub (+ tests)
frontend/src/
  lib/player.ts            WebSocket + MSE player (reconnects, latency control)
  components/StreamTile.tsx
  App.tsx                  URL form, grid, layout picker
scripts/mediamtx.yml       demo RTSP streams
Dockerfile                 backend + frontend + MediaMTX in one image
```

## Run locally

Requirements: Go 1.25 or newer, Bun (or Node 20+), FFmpeg, and MediaMTX (`brew install ffmpeg mediamtx`).

```bash
# 1. Test RTSP streams at rtsp://localhost:8554/{testsrc,smpte,life,hevc}
mediamtx scripts/mediamtx.yml

# 2. Backend on :8080
cd backend && go run .

# 3. Frontend on :5173 (proxies /ws and /api to :8080)
cd frontend && bun install && bun run dev
```

Open http://localhost:5173 and click **Load demo streams**, or paste your own RTSP URL.

### With Docker

```bash
docker build -t rtsp-viewer . && docker run -p 8080:8080 rtsp-viewer
# open http://localhost:8080
```

### Tests

```bash
cd backend && go test -race ./...   # unit + integration tests (the integration tests run real FFmpeg)
cd frontend && bun run build && bun run lint
```

## Configuration

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP port |
| `FFMPEG_PATH` | `ffmpeg` | FFmpeg binary |
| `MAX_STREAMS` | `16` | Maximum number of FFmpeg processes running at once |
| `IDLE_TIMEOUT_SEC` | `10` | How long a stream keeps running after its last viewer leaves |
| `STALL_TIMEOUT_SEC` | `15` | Restart FFmpeg if no data arrives for this long |
| `STATIC_DIR` | – | If set, the backend also serves the built frontend from this directory |
| `ALLOWED_ORIGINS` | `*` | Comma-separated WebSocket origin patterns |
| `DEMO_STREAMS` (Docker) | `1` | Start the bundled MediaMTX demo server |
| `VITE_BACKEND_URL` (frontend build) | same origin | Backend URL, for hosting the frontend separately (e.g. on Vercel) |
| `VITE_DEMO_STREAMS` (frontend build) | bundled demo URLs | Comma-separated URLs for the "Load demo streams" button |

## Deployment

### Vercel (live demo)

`Dockerfile.vercel` at the repo root is auto-detected by Vercel and deployed as a [container-image Function](https://vercel.com/docs/functions/container-images). It contains the Go backend, FFmpeg, the built React app and the demo MediaMTX server. Vercel Functions on Fluid Compute support [WebSockets](https://vercel.com/kb/guide/do-vercel-serverless-functions-support-websocket-connections) (public beta).

To deploy: import the repo at vercel.com/new with Root Directory = repo root, then click Deploy. No env vars are needed.

Platform limits and how the app handles them:

- WebSocket connections are closed at the function's maximum duration (300 s on Hobby). The player reconnects automatically, so this shows up as a brief "Reconnecting" blip.
- Each instance runs its own FFmpeg processes and demo server. Viewers are only shared within one instance.
- Instances scale down after 5 minutes without traffic; the next visit cold-starts.

### Railway / Render / Fly / any Docker host

`Dockerfile` is the same image listening on `:8080`. Deploy it as-is: it serves the frontend and the demo streams. Long-lived containers avoid the WebSocket duration limit.

### Frontend separately

Build `frontend/` with `VITE_BACKEND_URL=https://<backend-host>` (`frontend/vercel.json` covers this on Vercel), and set `ALLOWED_ORIGINS` on the backend to the frontend's domain.

## Notes and trade-offs

- Only video is streamed (`-an`); audio is dropped. This keeps fragments keyframe-aligned and simplifies the MSE setup.
- Many "public RTSP test URLs" found online are dead (NXDOMAIN, timeouts, refused). `rtsp://stream.strba.sk:1935/strba/VYHLAD_JAZERO.stream` (a lake webcam, H.264 720p, ~2.7 s GOP) worked when this was written and is included in the demo set. Use `ffprobe -rtsp_transport tcp <url>` to check whether a URL is reachable at all.
- Latency is about one GOP (≈1 s for the demo streams). Cameras with long GOPs will have proportionally higher latency. Lowering it further would need WebRTC, or fragments split mid-GOP plus keyframe tracking.
- The backend opens any RTSP URL a user submits. In production, add authentication and a host allowlist to prevent SSRF.
