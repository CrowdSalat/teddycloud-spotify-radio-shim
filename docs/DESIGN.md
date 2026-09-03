# Design: teddycloud-spotify-radio-shim

Read [REQUIREMENTS.md](REQUIREMENTS.md) first.  
For Spotify Soloist specifics, see [research/spotify-soloist.md](research/spotify-soloist.md).

---

## Blockers — must be resolved before implementation

Two things cannot be decided from documentation alone. They require a running Soloist binary.

### Blocker 1: Does `--single-track` accept album URIs?

The Soloist CLI flag `--single-track URI` is documented as "for one Spotify URI, not broader playback contexts." It never explicitly lists which URI types are accepted. The phrase "not broader playback contexts" most likely describes exit behaviour (plays the URI and exits), not a restriction to track-only URIs.

**Test:** run `soloist --single-track spotify:album:<id>` with a paired session. Observe whether playback starts or Soloist exits with code `1`.

| Outcome | Consequence |
|---|---|
| Album plays, Soloist exits when done | Single-track mode works for all URI types. Simplest design. |
| Soloist exits with code `1` | Albums and playlists require Spotify Connect mode + WebSocket `play` command. Slightly more complex process model (see below). |

### Blocker 2: Can audio be captured from a headless PulseAudio null sink?

Soloist outputs audio to PipeWire or PulseAudio only. There is no pipe or file output. The proposed approach is:

1. Run PulseAudio inside the container with a null sink as the default output device.
2. Soloist plays to the null sink.
3. The shim records from the null sink's `.monitor` source.

**Test:** run PulseAudio with a null sink in a container (no sound card), start Soloist against it, record from `virtual_out.monitor`, verify audio bytes arrive and are valid PCM.

Unknowns:
- Does Soloist accept a PulseAudio-only environment (no PipeWire available)?
- Does the null sink `.monitor` source produce audio with correct timing (real-time rate-limited)?
- Is `github.com/jfreymuth/pulse` sufficient, or is a subprocess recorder needed?

---

## Language and runtime

**Go.** The shim is written in Go. The existing codebase is Go. Soloist is a subprocess; the shim controls it via its WebSocket API.

---

## System overview

```
AUDIO (data plane):
  Toniebox ──HTTPS──▶ Teddycloud ──HTTP GET /stream?uri=──▶ Shim ◀── PulseAudio monitor ◀── Soloist ◀── Spotify CDN

CONTROL (event plane):
  Toniebox ──RTNL──▶ Teddycloud ──SSE──▶ Shim ──WebSocket──▶ Soloist
```

Teddycloud re-encodes the audio stream it receives. The shim does not need to produce a specific codec. It must produce a continuous HTTP stream that ffmpeg (inside Teddycloud) can decode. Raw PCM with a WAV header is sufficient.

---

## Container topology

**Open question:** whether all processes (PulseAudio, Soloist, shim) run in one container or across separate containers (e.g. one per process with a shared PulseAudio socket volume). The single-container approach is simpler for now and matches the existing pattern. Revisit if operational complexity warrants splitting.

The current working assumption is a **single container**, shown below.

## Components

```
┌────────────────────────── Container (single instance) ──────────────────────────┐
│                                                                                  │
│  tini (PID 1)                                                                    │
│    └── entrypoint script                                                         │
│          ├── starts pulseaudio (null sink, default output)                       │
│          └── starts shim                                                         │
│                                                                                  │
│  shim process                                                                    │
│    ├── Soloist supervisor  ── spawns/monitors soloist subprocess                 │
│    │     └── WebSocket client ── ws://127.0.0.1:<ws.port>                        │
│    ├── PulseAudio recorder ── records from virtual_out.monitor                   │
│    │     └── chunk channel ── buffers PCM chunks in memory                       │
│    ├── HTTP server                                                                │
│    │     ├── GET /stream?spotify_uri=...  ── audio delivery                      │
│    │     └── GET /healthz                                                         │
│    └── SSE listener ── Teddycloud /api/sse                                       │
│                                                                                  │
│  soloist subprocess                                                               │
│    ├── Spotify Connect session                                                   │
│    ├── WebSocket API on 127.0.0.1:<dynamic port>                                 │
│    └── audio output ──▶ PulseAudio (virtual_out null sink)                       │
│                                                                                  │
│  pulseaudio daemon                                                                │
│    └── null sink: virtual_out  (monitor source: virtual_out.monitor)             │
│                                                                                  │
│  PVC: /data  ── Soloist data directory (session, ws.port, ws.addr)               │
└──────────────────────────────────────────────────────────────────────────────────┘
```

---

## Soloist subprocess

### Runtime mode

**Single-track mode** is the primary target. The shim starts a new Soloist process for each URI request. Soloist plays the URI and exits.

If Blocker 1 shows that album/playlist URIs are rejected in single-track mode, the fallback is **Spotify Connect mode**: one long-running Soloist process, URI changes sent via WebSocket `play` command.

### Startup command

```
soloist \
  --device-name "teddycloud-spotify-shim" \
  --api-key "$SOLOIST_API_KEY" \
  --data-dir /data \
  --cache-dir /cache \
  --ws 127.0.0.1:0 \
  --single-track <URI>
```

`--ws 127.0.0.1:0` — OS picks the port. The shim reads the actual port from `/data/ws.port` after Soloist starts.

### WebSocket port discovery

Soloist writes `ws.addr` and `ws.port` into the data directory once the WebSocket server is up. The shim polls for the file's existence with a short timeout before connecting.

### Pairing (one-time setup)

Before single-track mode works, a session must be stored. The operator runs:

```
soloist --device-name "teddycloud-spotify-shim" --api-key "$SOLOIST_API_KEY" --data-dir /data --pair
```

Then opens the Spotify app and selects the device. The session is stored in `/data` (PVC). Subsequent starts restore it automatically.

### Build expiry

Soloist binaries expire after 90 days. Exit code `10` signals expiry. The shim supervisor must detect this exit code and emit a clear log message. The binary cannot be redistributed, so it must be downloaded by the user or at image build time into a private registry.

### Supervisor behaviour

The supervisor wraps the Soloist subprocess and handles:
- Startup: wait for `ws.port` file to appear before declaring ready
- Normal exit `0`: expected end of single-track playback, no restart
- Exit code `10`: log expiry error, do not restart, surface via `/healthz`
- Exit code `1`: log error, optionally retry with backoff
- Crash (signal): restart with exponential backoff

---

## Audio path

```
Soloist ──libpulse──▶ PulseAudio null sink
                              │
                      virtual_out.monitor
                              │
                   github.com/jfreymuth/pulse
                    RecordMonitor(virtual_out)
                              │
                        chunk channel
                        (in-process)
                              │
                        HTTP writer
                        WAV header + PCM stream
                              │
                        Teddycloud
                        (ffmpeg re-encodes to Opus/TAF)
                              │
                         Toniebox
```

### PulseAudio setup

PulseAudio runs inside the container with no physical sound card. Configuration:

```
load-module module-null-sink sink_name=virtual_out sink_properties=device.description="Shim_Sink"
set-default-sink virtual_out
```

Run with `--exit-idle-time=-1` so it does not quit when Soloist is between tracks.

### Recorder

The shim records from `virtual_out.monitor` using `github.com/jfreymuth/pulse` (pure Go, no cgo). It reads PCM s16le at 44100 Hz stereo and feeds chunks into a buffered channel. The channel decouples the recorder from the HTTP writer.

If Blocker 2 shows that the Go library is insufficient, the fallback is an `ffmpeg -f pulse -i virtual_out.monitor -f s16le pipe:1` subprocess with `StdoutPipe()`.

### HTTP stream format

The `/stream` endpoint writes a **streaming WAV header** (size fields set to `0xFFFFFFFF`) followed by raw PCM chunks from the channel. `Content-Type: audio/wav`. Teddycloud's ffmpeg accepts this and re-encodes it to Opus/TAF for the Toniebox.

### Backpressure

The recorder reads from PulseAudio at real-time speed regardless of consumer state. If the HTTP writer stalls (TCP back-pressure), the channel fills. The recorder discards chunks when the channel is full to prevent the PulseAudio client buffer from stalling. This is the same safety valve pattern used in the previous go-librespot design.

---

## Control path

```
Toniebox ──▶ Teddycloud SSE ──▶ Shim SSE listener ──▶ Soloist WebSocket
```

The shim connects to Teddycloud's SSE endpoint (`/api/sse`) and listens for hardware events. Each event maps to a WebSocket command sent to Soloist.

| Teddycloud SSE event | Soloist WebSocket command |
|---|---|
| Figurine placed | `{ "type": "command", "command": "play" }` |
| Figurine lifted | `{ "type": "command", "command": "pause" }` |
| Right ear slap | `{ "type": "command", "command": "skip_next" }` |
| Left ear slap | `{ "type": "command", "command": "skip_prev" }` |

The SSE listener must reconnect automatically if Teddycloud restarts or the connection drops.

---

## Hot-swap (figurine change)

When Teddycloud requests `/stream` with a new URI while a stream is already active:

1. Shim cancels the previous stream's HTTP response context.
2. Shim starts a new Soloist subprocess with the new URI (single-track mode), or sends `play` with the new URI via WebSocket (Connect mode).
3. The recorder keeps running — PulseAudio monitor source is always open.
4. The new HTTP response starts reading from the channel.

There is no FIFO, no pipe lock, no deadlock risk. The audio path is decoupled through PulseAudio and an in-process channel.

---

## Testability without Toniebox and Teddycloud

The shim must be testable with only `curl` and a mock SSE server.

### Audio test

```bash
curl http://localhost:8080/stream?spotify_uri=spotify:album:<id> | ffplay -f wav -
```

If audio plays in ffplay, the audio path is working.

### Control test

A `cmd/mock-teddycloud` binary (in this repo) serves a minimal SSE endpoint and a small web page with buttons that fire figurine-placed, figurine-lifted, right-ear, and left-ear events. The shim connects to the mock instead of real Teddycloud via the `TEDDYCLOUD_URL` env var.

---

## Configuration

All configuration via environment variables.

| Variable | Required | Default | Description |
|---|---|---|---|
| `SOLOIST_API_KEY` | Yes | — | Spotify Soloist API key. Treat as secret. |
| `TEDDYCLOUD_URL` | Yes | — | Teddycloud base URL, e.g. `http://teddycloud:80` |
| `LISTEN_ADDR` | No | `:8080` | Shim HTTP listen address |
| `SOLOIST_DATA_DIR` | No | `/data` | Soloist data directory. Mount PVC here. |
| `SOLOIST_CACHE_DIR` | No | `/cache` | Soloist cache directory |
| `SOLOIST_DEVICE_NAME` | No | `teddycloud-spotify-shim` | Spotify Connect device name |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |

---

## API surface

| Endpoint | Method | Description |
|---|---|---|
| `/stream` | `GET` | `?spotify_uri=<URI>` — starts playback, streams WAV audio. One active stream at a time. |
| `/healthz` | `GET` | Returns 200 if shim is running. Returns 503 with reason if Soloist exited with code 10 (expired). |

---

## Open questions (post-blocker)

These depend on the blocker test results but do not block further design work.

1. **Single-track vs Connect mode** — resolved by Blocker 1.
2. **PulseAudio vs PipeWire** — Soloist prefers PipeWire. If the null-sink approach works with PulseAudio alone (via `pipewire-pulse` compatibility layer or native PulseAudio), no PipeWire daemon is needed. If Soloist requires native PipeWire, the container needs PipeWire + WirePlumber + pipewire-pulse.
3. **Startup ordering** — PulseAudio must be ready before Soloist starts (Soloist connects to PulseAudio at launch). The entrypoint script must wait for the PulseAudio socket before starting Soloist.
4. **Seek on resume** — when a figurine is lifted and placed again, should playback resume from where it paused (default Spotify behaviour) or restart from the beginning? Default Spotify behaviour (resume from position) is assumed.
