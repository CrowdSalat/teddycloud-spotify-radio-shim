# Design: teddycloud-spotify-radio-shim

Read [REQUIREMENTS.md](REQUIREMENTS.md) first.  
Blocker results and Soloist specifics: [research/spotify-soloist.md](research/spotify-soloist.md).  
Incremental build order: [STRUCTURE.md](STRUCTURE.md).

---

## System overview

```
AUDIO (data plane):
  Toniebox ──HTTPS──▶ Teddycloud ──HTTP GET /stream?spotify_uri=──▶ Shim ◀── PulseAudio monitor ◀── Soloist ◀── Spotify CDN

CONTROL (event plane):
  Toniebox ──RTNL──▶ Teddycloud ──SSE──▶ Shim ──WebSocket──▶ Soloist
```

Teddycloud re-encodes whatever the shim sends. The shim does not need to produce a specific codec — raw PCM with a streaming WAV header is sufficient.

---

## Language and runtime

**Go.** Single binary. The shim is the process orchestrator — it starts and supervises PulseAudio and Soloist as subprocesses. There is no shell entrypoint script.

---

## URI delivery (Scenario A)

Teddycloud is configured per-figurine with a stream URL:

```
http://<shim>:8080/stream?spotify_uri=<URI>
```

The URI lives in Teddycloud's figurine config. The shim has no mapping table of its own. SSE events carry no URI — they are pure transport controls (play/pause/skip).

---

## Subprocess orchestration

The shim owns three subprocesses:

| Subprocess | Purpose |
|---|---|
| PulseAudio | Virtual audio sink. Runs headless, no sound card needed. |
| Soloist | Spotify Connect device. Plays to PulseAudio. Controlled via WebSocket. |
| Recorder | Reads PCM from `virtual_out.monitor`. Implemented as a goroutine, not a subprocess. |

All subprocess spawning is behind a `ProcessManager` interface so the state machine is unit-testable without binaries on `$PATH`.

Two additional interfaces keep audio backend concerns isolated:

- **`AudioDaemon`** — `Start()`, `Ready()`, `Stop()`. The PulseAudio implementation satisfies this. A future PipeWire implementation would satisfy the same interface. Nothing outside `internal/audio` touches PulseAudio directly.
- **`ChunkSource`** — produces `chan []byte`. The `github.com/jfreymuth/pulse` recorder satisfies this. The ffmpeg-subprocess fallback satisfies the same interface. The `/stream` handler never knows which backend is running.

To switch from PulseAudio to PipeWire later: implement `AudioDaemon` for PipeWire (different packages, different startup sequence, `pipewire` + `wireplumber` processes). The recorder does not change — `pipewire-pulse` makes PulseAudio protocol clients work against PipeWire transparently. Only the container packages and the daemon orchestration change.

### PulseAudio

Started by the shim before Soloist. Required args (verified — see research):

```
pulseaudio --exit-idle-time=-1 -n
  --load=module-native-protocol-unix
  --load=module-null-sink sink_name=virtual_out sink_properties=device.description=Shim_Sink
  --daemonize=yes --log-target=stderr
```

The shim sets `HOME` and `XDG_RUNTIME_DIR` to writable scratch dirs before spawning, then polls for the Unix socket before proceeding.

### Soloist

Not baked into the container image (redistribution concern). The shim checks `$PATH` and `$SOLOIST_DATA_DIR/bin/soloist` at startup. If not found, downloads from the Spotify CDN for the detected architecture.

Startup command:

```
soloist --device-name $SOLOIST_DEVICE_NAME --api-key $SOLOIST_API_KEY
        --data-dir $SOLOIST_DATA_DIR --cache-dir $SOLOIST_CACHE_DIR
        --ws 127.0.0.1:0
```

`--ws 127.0.0.1:0` — OS picks the port. The shim reads the actual port from `$SOLOIST_DATA_DIR/ws.port` once Soloist writes it, then opens the WebSocket and sends `activate`.

#### Supervisor exit-code handling

| Exit code | Meaning | Action |
|---|---|---|
| `0` | Unexpected clean exit | Log warning, restart with backoff |
| `1` | General failure | Log error, restart with backoff |
| `10` | Build expired | Log error, **do not restart**, surface via `/healthz` |
| Signal | Crash | Restart with exponential backoff |

Binaries expire after 90 days. On exit code `10`, the shim re-downloads Soloist on next startup.

#### Pairing (one-time setup)

Before Connect mode works, the operator runs Soloist manually with `--pair`, selects the device in the Spotify app, and the session is stored in `$SOLOIST_DATA_DIR`. Subsequent starts restore it automatically.

---

## Audio path

```
Soloist ──libpulse──▶ virtual_out (null sink)
                              │
                    virtual_out.monitor
                              │
                  github.com/jfreymuth/pulse
                        (goroutine)
                              │
                        chan []byte
                              │
                       /stream handler
                   WAV header + PCM chunks
                              │
                         Teddycloud
                    (ffmpeg → Opus/TAF)
                              │
                          Toniebox
```

The recorder runs continuously and independently of HTTP clients. Chunks are discarded when the channel is full (backpressure safety valve) to prevent the PulseAudio client buffer from stalling.

**Fallback:** if `github.com/jfreymuth/pulse` is insufficient, replace the recorder goroutine with `ffmpeg -f pulse -i virtual_out.monitor -f s16le -ar 44100 -ac 2 pipe:1` via `StdoutPipe()`. The `chan []byte` interface is unchanged.

### WAV header

Write a streaming WAV header with both RIFF and data size fields set to `0xFFFFFFFF`. Do not compute `0xFFFFFFFF + 36` — that overflows the 32-bit field and delivers 0 bytes.

---

## Control path

The shim connects to `$TEDDYCLOUD_URL/api/sse` and translates events to WebSocket commands. Auto-reconnects on drop.

| SSE event | WebSocket command |
|---|---|
| Figurine placed | `{ "type": "command", "command": "play" }` |
| Figurine lifted | `{ "type": "command", "command": "pause" }` |
| Right ear slap | `{ "type": "command", "command": "skip_next" }` |
| Left ear slap | `{ "type": "command", "command": "skip_prev" }` |

---

## Hot-swap

When `/stream` is called with a new URI while a stream is active:

1. Cancel the previous stream's HTTP response context.
2. Send WebSocket `play` with the new URI.
3. Recorder keeps running — monitor source is always open.
4. New HTTP response reads from the channel.

One active stream slot, protected by a mutex.

---

## Container

Single container. Debian trixie base (glibc ≥ 2.38 required; Bookworm ships 2.36). Install `pulseaudio libatomic1 tini ca-certificates`. `tini` is PID 1. The shim binary is the sole entrypoint — no shell script.

```
ENTRYPOINT ["/usr/bin/tini", "--", "/shim"]
```

Soloist is downloaded at runtime by the shim, not at image build time.

---

## API surface

| Endpoint | Method | Description |
|---|---|---|
| `/stream` | `GET` | `?spotify_uri=<URI>` — starts playback, streams WAV. One active stream at a time. |
| `/healthz` | `GET` | `200` if healthy. `503 {"error":"soloist_expired"}` if Soloist exited with code 10. |

---

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `SOLOIST_API_KEY` | Yes | — | Spotify Soloist API key. Treat as secret. |
| `TEDDYCLOUD_URL` | Yes | — | Teddycloud base URL, e.g. `http://teddycloud:80` |
| `LISTEN_ADDR` | No | `:8080` | Shim HTTP listen address |
| `SOLOIST_DATA_DIR` | No | `/data` | Soloist data + session directory. Mount PVC here. |
| `SOLOIST_CACHE_DIR` | No | `/cache` | Soloist cache directory |
| `SOLOIST_DEVICE_NAME` | No | `teddycloud-spotify-shim` | Spotify Connect device name |
| `SOLOIST_BIN` | No | auto | Explicit path to soloist binary. Skips auto-download if set. |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |
