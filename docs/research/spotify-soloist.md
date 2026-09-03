# Research: Spotify Soloist

Source: https://developer.spotify.com/documentation/soloist  
Fetched: 2026-09-02

---

## What it is

Spotify Soloist is a **closed-source binary daemon** distributed by Spotify. It is a headless Spotify Connect receiver for Linux. It is not a library and not a Go package. Your code interacts with it via its local **WebSocket API** or the companion `soloist ctl` CLI.

The binary is downloaded from Spotify's CDN. No source is published.

---

## Download

| Architecture | URL |
|---|---|
| x86_64 | `https://soloist-builds.spotifycdn.com/soloist_release_x86_64.tar.gz` |
| aarch64 | `https://soloist-builds.spotifycdn.com/soloist_release_arm64.tar.gz` |
| armv7 | `https://soloist-builds.spotifycdn.com/soloist_release_arm32.tar.gz` |

Spotify prohibits redistributing the binary or archives. Each deployer must download from the CDN directly.

### Build expiry

Each build expires **90 days** after its build date. Soloist logs remaining lifetime at startup and exits with code `10` when expired. The container image must re-download the binary before expiry, or be rebuilt.

---

## Authentication

Two separate pieces of state are required:

### API key

- Generated at https://developer.spotify.com/dashboard/soloist
- The account must have **Spotify Premium**
- Required at every startup, including session restores and single-track mode
- Must not be shared, published, or committed to source control
- Passed via `--api-key "$SOLOIST_API_KEY"` — prefer an env var from a secret manager

### Spotify Connect session

Soloist does not open a browser or prompt for a username/password. Login happens through the Spotify app:

1. Start Soloist with a device name and API key
2. Open the Spotify app on the same LAN
3. Select the Soloist device from the device picker
4. Soloist stores the session in its data directory

On subsequent starts, Soloist restores the stored session automatically. The session survives restarts as long as the data directory is preserved.

**Re-pairing:** run with `--pair` to replace the stored session.

**Single-track mode** requires a stored session. Run `--pair` once first, then use `--single-track`.

---

## Runtime modes

| Mode | Flag | Behaviour |
|---|---|---|
| **Spotify Connect device** (default) | — | Advertises on LAN, waits for Spotify app to connect, stays running |
| **Single-track mode** | `--single-track URI` | Restores stored session, plays one URI, exits when done |

Single-track mode:
- Does not advertise a new Spotify Connect device
- Starts with shuffle and repeat disabled
- Remote control and playback transfer disabled for that context
- Exits when the item finishes, stops, or changes

**For this project:** single-track mode is the intended runtime mode. The shim starts Soloist per-URI and captures the audio output.

### Accepted URI types in single-track mode

The docs say `--single-track` is "for one Spotify URI, not broader playback contexts." Exit code `1` includes "invalid single-track URI" as a failure reason.

The phrase "not broader playback contexts" describes **behaviour**, not URI type filtering. It means: Soloist exits after the URI finishes; it does not continue playing the surrounding playlist or album queue. It does not mean album or playlist URIs are rejected.

The WebSocket `play` command explicitly lists "track, album, playlist, or episode" as accepted types. The `--single-track` docs never enumerate accepted types.

**Working hypothesis:** `--single-track` accepts any single playable URI including albums and playlists. An album URI plays the album and Soloist exits when the album ends.

**Must be verified empirically** — pass an album URI (`spotify:album:...`) to `--single-track` and observe whether it starts playback or exits with code `1`.

---

## Audio output

Soloist plays through **PipeWire or PulseAudio only**. No ALSA backend.

- PipeWire is used if it initialises successfully
- Falls back to PulseAudio otherwise
- `--pipewire-device DEVICE` routes to a specific PipeWire node name or ID (PipeWire only)

There is no pipe or file output. Audio goes to the audio system. To capture it, the audio system must be configured with a virtual/null sink and the monitor source recorded.

---

## WebSocket API

Enable with `--ws ADDR:PORT`. For local-only use, bind to `127.0.0.1`. Use port `0` to let the OS pick a free port.

When port `0` is used, Soloist writes the actual address and port to files in the data directory:

| File | Contents |
|---|---|
| `ws.addr` | Bind address |
| `ws.port` | Actual listening port |

`soloist ctl` uses these files for auto-discovery. Files are removed on shutdown.

**Security:** No built-in auth, TLS, Origin validation, or CSRF protection. Local-only use is assumed.

### Message format

All messages are JSON text frames.  
Client → server: must have `"type": "command"`.  
Server → client: every event has a `"type"` field.

### Events (server → client)

| Event | When sent |
|---|---|
| `auth_state` | On connect, on auth change, in response to `get_auth_state` |
| `playback_state` | On connect (if logged in), in response to `get_state`, on broad state change |
| `track_changed` | Track changes |
| `playback_changed` | Status changes (`idle`, `playing`, `paused`, `buffering`) |
| `volume_changed` | Volume changes |
| `device_changed` | Active device changes |
| `context_changed` | Playback context changes |
| `options_changed` | Shuffle/repeat/speed changes |
| `position_sync` | Position anchor changes (use to interpolate locally) |
| `queue_changed` | Queue changes, in response to `get_queue` |
| `command_result` | Echoes accepted command name |
| `error` | Malformed message, unknown command, unauthenticated access |

`command_result` means the command was accepted, not that the state has changed yet. State changes arrive as separate events.

### Commands (client → server)

#### Query commands (no `command_result`, returns the event directly)

| Command | Auth required | Description |
|---|---|---|
| `get_auth_state` | No | Request `auth_state` |
| `get_state` | Yes | Request `playback_state` |
| `get_queue` | Yes | Request `queue_changed`. Optional `limit` field |

#### Control commands

| Command | Fields | Description |
|---|---|---|
| `play` | Optional `uri` | Resume or play a Spotify URI (track, album, playlist, episode) |
| `pause` | — | Pause playback |
| `skip_next` | — | Skip to next track |
| `skip_prev` | — | Skip to previous or restart current |
| `seek` | `position_ms` | Seek to position in ms |
| `set_volume` | `volume` (0–100) | Set volume |
| `set_shuffle` | `enabled` bool | Enable/disable shuffle |
| `set_repeat_context` | `enabled` bool | Enable/disable context repeat |
| `set_repeat_track` | `enabled` bool | Enable/disable track repeat |
| `add_to_queue` | `uri` (track only) | Add track URI to queue |
| `activate` | — | Make Soloist the active Spotify Connect device |
| `deactivate` | — | Give up active-device status |

All control commands require a logged-in session.

Example messages:
```json
{ "type": "command", "command": "play", "uri": "spotify:playlist:37i9dQZF1DXcBWIGoYBM5M" }
{ "type": "command", "command": "pause" }
{ "type": "command", "command": "skip_next" }
{ "type": "command", "command": "seek", "position_ms": 30000 }
```

### Toniebox control mapping

| Toniebox action | Soloist command |
|---|---|
| Figurine placed | `play` (resume) |
| Figurine lifted | `pause` |
| Right ear slap | `skip_next` |
| Left ear slap | `skip_prev` |

---

## CLI reference summary

### Required flags

| Flag | Description |
|---|---|
| `-n, --device-name NAME` | Spotify Connect device name |
| `-k, --api-key KEY` | API key. Treat as a secret |

### Storage flags

| Flag | Description |
|---|---|
| `-D, --data-dir PATH` | Persistent data (session, PID, ws files). Mount a PVC here |
| `-C, --cache-dir PATH` | Volatile playback cache |
| `-z, --cache-size MB` | Max cache size. 0 = no limit. Minimum 100 if set |

### Playback flags

| Flag | Description |
|---|---|
| `-d, --pipewire-device DEVICE` | Route to specific PipeWire node name or ID |
| `-i, --initial-volume N` | Initial volume (0–100) |
| `-s, --single-track URI` | Play one URI and exit |
| `-p, --pair` | Pair via Spotify Connect, store session, exit |

`--pair` and `--single-track` are mutually exclusive.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success (includes `--pair`, `--single-track`, normal shutdown) |
| `1` | General failure (invalid args, missing session, playback failure, etc.) |
| `10` | Build expired — must download a newer build |

---

## Audio capture: the core design challenge

Soloist has **no pipe or file output**. It plays to PipeWire or PulseAudio exclusively. To get audio as a byte stream, the audio system must be set up so Soloist's output can be recorded.

### Approach: PulseAudio null sink + monitor source

PulseAudio can run headless (no display, no sound card). A **null sink** is a software-only sink that discards output but exposes a `.monitor` source that can be recorded.

```
Soloist → PulseAudio null sink → virtual_out.monitor → [recorder] → byte stream
```

**Setting up the null sink:**
```
load-module module-null-sink sink_name=virtual_out
set-default-sink virtual_out
```

**Recording from the monitor (raw PCM):**
```bash
parec --device=virtual_out.monitor --format=s16le --rate=44100 --channels=2
```

**Recording via ffmpeg:**
```bash
ffmpeg -f pulse -i virtual_out.monitor -f s16le -ar 44100 -ac 2 pipe:1
```

**Recording via Go** (`github.com/jfreymuth/pulse` — pure Go, no cgo):
```go
client, _ := pulse.NewClient()
sink, _   := client.SinkByID("virtual_out")
stream, _ := client.NewRecord(
    pulse.NewWriter(w, proto.FormatInt16LE),
    pulse.RecordMonitor(sink),
    pulse.RecordStereo,
    pulse.RecordSampleRate(44100),
)
stream.Start()
```

### Approach: PipeWire null sink + monitor source

PipeWire also runs headless. The equivalent of a null sink is the `support.null-audio-sink` factory.

```bash
# runtime creation
pw-cli create-node adapter \
  '{ factory.name=support.null-audio-sink node.name=virtual-out media.class=Audio/Sink }'
```

Recording:
```bash
pw-cat --record --target=virtual-out.monitor --format=s16 --rate=44100 --channels=2 -
ffmpeg -f pipewire -i virtual-out.monitor -f s16le -ar 44100 -ac 2 pipe:1
```

**`pipewire-pulse`:** PipeWire ships a PulseAudio-protocol compatibility layer. With it running, PulseAudio tools (`pactl`, `parec`) and Go PulseAudio libraries work against PipeWire transparently. Running just PipeWire + pipewire-pulse covers both.

### OpenShift / container security

Both PulseAudio and PipeWire run entirely in **userspace**. No kernel modules are needed. Both communicate via Unix sockets under `$XDG_RUNTIME_DIR`. Both are compatible with OpenShift `restricted-v2` SCC.

The ALSA loopback alternative (`snd_aloop`) requires loading a kernel module and is not viable in restricted container environments.

### Capture comparison

| Method | Library dep in shim | Container complexity | Notes |
|---|---|---|---|
| `github.com/jfreymuth/pulse` | Go, no cgo | PulseAudio daemon in container | Cleanest Go integration |
| `ffmpeg -f pulse` subprocess | None (ffmpeg binary) | PulseAudio daemon in container | Same ffmpeg already in container for Teddycloud comparison |
| `pw-cat` subprocess | None | PipeWire + WirePlumber in container | More processes to manage |

**Recommendation:** PulseAudio null sink + `github.com/jfreymuth/pulse` for direct Go integration, with `pipewire-pulse` optionally layered on top if Soloist requires PipeWire on the target system.

---

## Runtime environment requirements (verified)

Verified against Soloist v1.3.8 (build 20260902):

| Requirement | Detail |
|---|---|
| **glibc >= 2.38** | Bookworm ships 2.36 — too old. Use Debian **trixie** or Ubuntu 24.04 as the base image. |
| **libatomic.so.1** | Not present in all base images. Install `libatomic1` explicitly. |
| **PulseAudio null sink** | Confirmed working: `virtual_out` at s16le 44100Hz stereo, `virtual_out.monitor` available for recording. |
| **PulseAudio startup** | Must use `-n --file=` to load only required modules. Without `-n`, the system default config conflicts. No D-Bus required. |

---

## Constraints and open questions

| Item | Detail |
|---|---|
| **90-day expiry** | Container image or update mechanism must refresh the binary before expiry. Expiry check on startup (exit code `10`) must be handled by the shim supervisor |
| **No redistribution** | Binary cannot be baked into a public container image. Must be downloaded at container start or as part of a private build |
| **Single-track URI types** | Docs say "not broader playback contexts" but never list forbidden URI types. Working hypothesis: album and playlist URIs are accepted, "not broader contexts" describes exit behaviour only. **Needs empirical verification.** Fallback: Connect mode + WebSocket `play` command covers all URI types definitively |
| **Session persistence** | Data directory must survive pod restarts (PVC mount). Removing it forces re-pairing |
| **No config file** | All config is CLI flags. The shim must construct and manage the command line |
| **WS port discovery** | Using `--ws 0` means the shim must read `ws.port` from the data directory after Soloist starts |
| **Audio system in container** | PulseAudio or PipeWire must run inside the container alongside Soloist and the shim. Adds processes and startup ordering |
