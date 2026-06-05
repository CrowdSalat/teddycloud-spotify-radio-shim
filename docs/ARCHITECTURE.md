# Architecture

Solution design for the teddycloud-spotify-radio-shim. Read [PRD.md](PRD.md) first for requirements.

For decision rationale, see the [ADRs](adr/).

---

## System Overview

The shim is a stateless bridge between Teddycloud and Spotify. It has two data paths:

```
AUDIO (data plane):
  Toniebox ──HTTPS──▶ Teddycloud ──HTTP GET──▶ Shim /stream?uri=... ◀──FIFO── go-librespot ◀── Spotify CDN

CONTROL (event plane):
  Toniebox ──RTNL──▶ Teddycloud ──SSE──▶ Shim ──REST API──▶ go-librespot
```

The shim does **not** control the Toniebox. The RTNL protocol is one-way (box → server). The shim controls **Spotify** in response to Toniebox hardware events.

---

## Components

```
┌──────────────────────── OpenShift Pod (replicas: 1) ────────────────────────┐
│                                                                             │
│  ┌─────────────────────────────── Container ──────────────────────────────┐  │
│  │                                                                       │  │
│  │  tini (PID 1)                                                         │  │
│  │    └── shim process                                                   │  │
│  │          ├── Subprocess Manager ── spawns/monitors go-librespot       │  │
│  │          ├── FIFO Reader ── /tmp/spotify.fifo ── chan(32 × 8KB)       │  │
│  │          ├── HTTP Server                                              │  │
│  │          │     ├── GET  /stream?spotify_uri=... ── audio delivery     │  │
│  │          │     └── GET  /login ── OAuth flow proxy                    │  │
│  │          ├── SSE Listener ── Teddycloud /api/sse                      │  │
│  │          └── WebSocket Listener ── go-librespot /events               │  │
│  │                                                                       │  │
│  │  go-librespot (child process)                                         │  │
│  │    ├── Spotify Connect session                                        │  │
│  │    ├── REST API on localhost:3678                                     │  │
│  │    ├── WebSocket /events on localhost:3678                             │  │
│  │    └── Pipe output → /tmp/spotify.fifo (s16le, 44.1kHz, stereo)      │  │
│  │                                                                       │  │
│  └───────────────────────────────────────────────────────────────────────┘  │
│                                                                             │
│  PVC: /config ── go-librespot state.json (session persistence)              │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Decisions:** [ADR-001](adr/001-spotify-engine.md) (go-librespot), [ADR-002](adr/002-container-pattern.md) (subprocess).

---

## Data Flow: Audio Path

```
go-librespot                    Shim                                    Teddycloud
┌──────────┐    Named FIFO     ┌────────────┐    Buffered Chan     ┌────────────┐
│ Vorbis   │   /tmp/spotify    │   FIFO     │    32 × 8KB slots   │  HTTP      │
│ decoder  ├──────────────────▶│   Reader   ├─────────────────────▶│  Writer    ├──▶ Toniebox
│          │    64 KB kernel   │ goroutine  │      256 KB          │ goroutine  │
│ 8KB/iter │    buffer         │            │                      │            │
└──────────┘                   └────────────┘                      └────────────┘
     ▲                              │                                    │
     │                         on overflow:                         blocks on TCP
     │                         discard (safety                     (gates real-time
  rate-limited                  valve)                              consumption)
  by FIFO
  backpressure
```

### Three buffers

| Buffer | Size | Purpose | On overflow |
|---|---|---|---|
| **FIFO** (kernel) | 64 KB (371 ms) | Rate-limiter. Mostly full during playback — throttles go-librespot to real-time via backpressure. | go-librespot `Write()` blocks briefly (~46ms). Normal. |
| **Channel** (in-process) | 256 KB / 32 slots (1.5 s) | Decouples FIFO reader from HTTP writer. Absorbs jitter. | Reader discards via `select/default` — safety valve preventing deadlock. |
| **TCP** (OS) | ~20-87 KB | Transparent. Go `Write()` blocks when full. | HTTP writer blocks — this is what causes channel to fill. |

**Decision:** [ADR-005](adr/005-fifo-backpressure.md) (backpressure-safe reader).

### States

| State | FIFO | Channel | go-librespot | Reader | HTTP Writer |
|---|---|---|---|---|---|
| **Playing** | Mostly full (normal) | Partial, flowing | Decoding at ~1× real-time | At consumer speed | At real-time |
| **Paused** | Empty | Drains → empty | Stopped (`cond.Wait`) | Blocked on empty FIFO | Stopped |
| **Stalled** | Partial (reader active) | Full → discard | Still decoding | Discarding ⚠️ | Blocked on TCP |
| **Skip** | Stale data consumed | Flushed on `will_play` | Switches track internally | Keeps reading | Gets fresh data |

---

## Data Flow: Control Path

```
Teddycloud                         Shim                          go-librespot
┌──────────┐    SSE /api/sse      ┌──────────────┐    REST API   ┌──────────┐
│          ├─────────────────────▶│ SSE Listener │──────────────▶│          │
│  RTNL    │  text/event-stream  │              │  localhost    │  Player  │
│  events  │                     │  event→action│  :3678       │  API     │
└──────────┘                     │  mapping     │               └──────────┘
                                 └──────────────┘
```

**Decision:** [ADR-003](adr/003-control-channel.md) (SSE), [ADR-004](adr/004-playback-control.md) (unified API).

### Event mapping

| Teddycloud SSE Event | Shim Action | go-librespot API |
|---|---|---|
| Tag placed (figurine down) | Resume | `POST /player/resume` |
| Tag removed (figurine lifted) | Pause | `POST /player/pause` |
| KnockForward (right slap) | Skip next | `POST /player/next` |
| KnockBackward (left slap) | Skip prev | `POST /player/prev` |

### Hot-swap (PRD §3.1)

When a new `GET /stream?uri=NEW` arrives while a previous stream is active:

1. `POST /player/play {"uri": "NEW"}` — go-librespot stops old track, starts new.
2. Close old HTTP response.
3. Reader goroutine keeps reading (never stops).
4. Wait for WebSocket `will_play` for new URI — flush channel of stale data.
5. Start forwarding channel to new HTTP response.

---

## API Surface

| Endpoint | Method | Purpose |
|---|---|---|
| `/stream` | `GET` | `?spotify_uri=<URI>` — starts playback, streams audio. One active stream at a time. |
| `/login` | `GET` | Proxies go-librespot's interactive OAuth flow. Shows clickable Spotify auth link. |
| `/healthz` | `GET` | Liveness probe. Returns 200 if shim is running. |
| `/readyz` | `GET` | Readiness probe. Returns 200 if go-librespot API is reachable and `playback_ready: true`. |

---

## Configuration

All via environment variables:

| Variable | Required | Default | Description |
|---|---|---|---|
| `TEDDYCLOUD_URL` | Yes | — | Base URL of Teddycloud (e.g., `http://teddycloud:80`) |
| `LISTEN_ADDR` | No | `:8080` | Shim HTTP listen address |
| `LIBRESPOT_CONFIG_DIR` | No | `/config` | go-librespot config directory (mount PVC here) |
| `CONTROL_CHANNEL` | No | `sse` | Event source: `sse` or `mqtt` |
| `MQTT_BROKER_URL` | If mqtt | — | MQTT broker URL (e.g., `tcp://mosquitto:1883`) |
| `MQTT_TOPIC_PREFIX` | If mqtt | `teddyCloud` | MQTT topic prefix |
| `LOG_LEVEL` | No | `info` | Log level: `debug`, `info`, `warn`, `error` |

---

## Implementation Notes

Findings confirmed during development that affect design or operation.

### FIFO open ordering is strict

The named FIFO read end must be opened before go-librespot opens the write end. go-librespot opens the pipe with `O_WRONLY|O_NONBLOCK` and returns an error immediately if no reader exists. `supervisor.Prepare()` creates the FIFO, then the caller must start a goroutine that opens the read end before calling `supervisor.Run()`. Opening in the same goroutine before `Run()` deadlocks — `os.Open` blocks waiting for a writer that never comes because `Run()` is never reached.

### go-librespot `GET /` blocks until authenticated

The root endpoint does not return until Spotify authentication completes. It cannot be used as a readiness check in unauthenticated state. The supervisor uses a **raw TCP dial on port 3678** instead — any successful connection means the API server is up and accepting connections, regardless of auth state.

### go-librespot ignores SIGTERM during auth

While blocked waiting for OAuth completion, go-librespot does not respond to SIGTERM. The supervisor's crash simulation test requires SIGKILL. In normal authenticated operation SIGTERM works correctly. The supervisor already sends SIGTERM first with a 5s grace period before escalating to SIGKILL.

### OAuth callback port is random and unexposed

go-librespot's interactive auth binds its callback server to `http://127.0.0.1:<random-port>/login`. The port changes on every restart. In a container, this port is never exposed to the host. Until Phase 7 implements the `/login` proxy, completing auth requires exec-ing into the container:

```bash
# Spotify redirects browser to: http://127.0.0.1:<PORT>/login?code=<CODE>
# Take that full URL and run it inside the container:
podman exec <container> curl -s "http://127.0.0.1:<PORT>/login?code=<CODE>"
# Response: "Go back to go-librespot!"
```

Phase 7 must expose a stable `/login` endpoint on port 8080 that discovers go-librespot's random callback port and proxies the OAuth code to it.

---

## Open Questions

1. **PCM → audio transcoding.** go-librespot outputs raw PCM. What Content-Type does Teddycloud accept? Needs testing against actual Teddycloud. Candidates: raw PCM with WAV header, OGG/Vorbis re-encoding, or passthrough if Teddycloud accepts raw.
2. **Tag-removed SSE event name.** `tbs_tag_removed()` emits an SSE event — exact name needs verification from Teddycloud source.
