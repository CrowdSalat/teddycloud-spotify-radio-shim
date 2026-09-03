# Structure: teddycloud-spotify-radio-shim

Read [DESIGN.md](DESIGN.md) first — it defines the architecture, decisions, and configuration.  
Read [REQUIREMENTS.md](REQUIREMENTS.md) for the problem context.

Each phase produces something runnable and testable before the next phase starts.  
Do not start a phase until the previous one passes its verification step.

---

## Testing strategy

Pure logic (config parsing, WAV header, SSE event parsing, exit-code state machine) is **unit tested** with no subprocess on `$PATH`.

Subprocess orchestration (PulseAudio, Soloist) is **integration tested** inside the container. Testing it with fakes gives false confidence because the real surface is binary behaviour, not the Go interface.

To keep the orchestration logic unit-testable, subprocess spawning is hidden behind a `ProcessManager` interface. The production implementation uses `exec.Cmd`. Tests inject a fake that returns canned exit codes and stdout.

---

## Phase 1 — Skeleton: Go module, config, HTTP, Containerfile

**Goal:** the shim compiles, passes lint, serves `/healthz`, and runs inside a container.

### Tasks

- `go mod init github.com/janharings/teddycloud-spotify-radio-shim`
- Package layout:
  ```
  cmd/shim/            main entry point — wires concrete types to interfaces
  cmd/mock-teddycloud/ mock SSE server (stubbed, implemented in Phase 6)
  internal/config/     env-var config loader
  internal/process/    ProcessManager interface + exec implementation
  internal/audio/      AudioDaemon + ChunkSource interfaces; PulseAudio implementation
  internal/recorder/   PulseRecorder + FFmpegRecorder — implement audio.ChunkSource
  internal/soloist/    Soloist supervisor + WebSocket client
  internal/server/     HTTP server — consumes audio.ChunkSource
  internal/sselistener/ SSE client
  ```
- `internal/config`: typed struct, env-var loading, defaults, fail-fast on missing required vars. Unit tested.
- `internal/process`: define `ProcessManager` interface (start, wait, kill). Implement with `exec.Cmd`. Provide a `Fake` implementation for tests.
- `internal/audio`: define `AudioDaemon` interface — `Start() error`, `Ready() bool`, `Stop()`. Define `ChunkSource` interface — `Chunks() <-chan []byte`. No implementations yet — those come in Phases 2 and 3.
- `internal/server`: HTTP server on `LISTEN_ADDR`. `/healthz` returns `200 OK`. No other routes yet.
- `cmd/shim/main.go`: load config, start server, block.
- `Containerfile`: Debian trixie base, install `pulseaudio libatomic1 tini ca-certificates`. Copy shim binary. `USER 65534:0`. `ENTRYPOINT ["/usr/bin/tini", "--", "/shim"]`.
- `Makefile` targets: `build`, `lint`, `test`, `container-build`.
- `.golangci.yml` already present — verify it runs clean on the new code.

### Verification

```bash
# local
go build ./...
go test ./...
golangci-lint run

# container
podman build --platform linux/amd64 --manifest shim:dev -f Containerfile .
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  shim:dev
curl -s http://localhost:8080/healthz   # → 200 OK
```

---

## Phase 2 — PulseAudio orchestration

**Goal:** the shim starts PulseAudio as a subprocess and verifies it is ready.

### Tasks

- `internal/audio`: implement `AudioDaemon` with a `PulseAudio` concrete type wrapping a `ProcessManager`.
  - Before spawning: set `HOME` and `XDG_RUNTIME_DIR` to writable scratch dirs (`/tmp/home`, `/tmp/runtime`) via `os.Setenv`. This avoids "Failed to create secure directory" when running with `--userns=keep-id`.
  - Start PulseAudio:
    ```
    pulseaudio --exit-idle-time=-1 -n
      --load=module-native-protocol-unix
      --load=module-null-sink sink_name=virtual_out sink_properties=device.description=Shim_Sink
      --daemonize=yes --log-target=stderr
    ```
    Note: `module-native-protocol-unix` must be loaded explicitly with `-n`; without it no client socket is created and all clients fail with "Connection refused".
  - Poll for the PulseAudio Unix socket at `$XDG_RUNTIME_DIR/pulse/native` until it appears (timeout: 10 s).
  - Expose a `Ready() bool` method.
- `/healthz` reflects PulseAudio state: `503` with reason if PulseAudio failed to start.
- Unit test: fake `ProcessManager` returns exit 0 immediately; assert `Ready()` is false and error is returned.

### Verification

Inside the container:

```bash
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  shim:dev
# shim log should show: "PulseAudio ready"
curl -s http://localhost:8080/healthz   # → 200 OK

# verify PulseAudio is actually up inside the container
podman exec <ctr> pactl info
```

---

## Phase 3 — Null sink + recorder

**Goal:** the shim creates the virtual audio sink and reads PCM bytes from its monitor source. No Soloist yet — silence is fine.

### Tasks

- `internal/audio`: `NullSink` setup (can be done via the PulseAudio load args from Phase 2 — verify the sink exists with `pactl list sinks short`).
- `internal/recorder`: implement `audio.ChunkSource` with a `PulseRecorder` concrete type using `github.com/jfreymuth/pulse` (pure Go, no cgo). `internal/recorder` imports `internal/audio` for the interface — not the other way around.
  - Format: `s16le`, 44100 Hz, stereo.
  - `Chunks()` returns a buffered `chan []byte`.
  - **Backpressure safety valve:** discard chunks when the channel is full. Prevents the PulseAudio client buffer from stalling.
  - Recorder runs continuously and independently of any HTTP client.
- Unit test: inject a fake reader; assert chunks arrive via `Chunks()` and that full-channel discards do not block.

> **Fallback note:** if `github.com/jfreymuth/pulse` proves insufficient, implement `FFmpegRecorder` in the same package — it runs `ffmpeg -f pulse -i virtual_out.monitor -f s16le -ar 44100 -ac 2 pipe:1` via `StdoutPipe()` and satisfies `audio.ChunkSource`. `cmd/shim/main.go` swaps which concrete type it wires in. Nothing else changes.

### Verification

Inside the container:

```bash
# shim log should show: "recorder started, reading from virtual_out.monitor"
# verify bytes arrive (silence from empty sink is fine at this stage):
podman exec <ctr> parec --device=virtual_out.monitor --format=s16le | head -c 1024 | wc -c
# → 1024  (bytes are flowing)
```

---

## Phase 4 — Soloist orchestration

**Goal:** the shim manages the full Soloist lifecycle and holds an open WebSocket connection.

### Tasks

- `internal/soloist`: `Supervisor` struct.
  - **Binary check:** look for `soloist` on `$PATH`, then at `$SOLOIST_DATA_DIR/bin/soloist`. If not found, download from the Spotify CDN for the detected architecture (`runtime.GOARCH`) using `net/http`. Place in `$SOLOIST_DATA_DIR/bin/soloist`, `chmod +x`.
  - **Expiry note:** log the binary's modification time at startup. If older than 80 days, log a warning. Re-download on next startup if exit code `10` is received.
  - Spawn Soloist:
    ```
    soloist --device-name $SOLOIST_DEVICE_NAME --api-key $SOLOIST_API_KEY
            --data-dir $SOLOIST_DATA_DIR --cache-dir $SOLOIST_CACHE_DIR
            --ws 127.0.0.1:0
    ```
  - Poll `$SOLOIST_DATA_DIR/ws.port` until it appears (timeout: 15 s).
  - Open WebSocket to `ws://127.0.0.1:<ws.port>`. Send `activate` command once connected.
  - Exit-code handling:
    - `0`: unexpected — log warning, restart with exponential backoff.
    - `1`: log error, restart with backoff.
    - `10`: log expiry error, **do not restart**, set state to `expired`.
    - Signal/crash: restart with backoff.
- `/healthz`: `503 {"error":"soloist_expired"}` when state is `expired`.
- Unit test: fake `ProcessManager` emits exit code `10`; assert state becomes `expired` and restart is not attempted.

### Verification

Inside the container (requires a valid paired session in `/data`):

```bash
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY -e TEDDYCLOUD_URL=http://localhost \
  -v ./container/soloist-data:/data:Z \
  shim:dev
# shim log: "soloist ready, WebSocket connected on port <n>"
curl -s http://localhost:8080/healthz   # → 200 OK
```

---

## Phase 5 — `/stream` endpoint

**Goal:** `curl /stream | ffplay` plays Spotify audio end-to-end.

### Tasks

- `internal/server`: register `GET /stream`. The handler depends on `audio.ChunkSource`, not on any concrete recorder type. `internal/server` imports `internal/audio` — it never imports `internal/recorder`.
  - Parse `?spotify_uri=` query param. Return `400` if missing or malformed.
  - Send WebSocket `play` command with the URI to Soloist.
  - Write **streaming WAV header**: RIFF and data size fields both set to `0xFFFFFFFF`. Do **not** compute `0xFFFFFFFF + 36` — that overflows the 32-bit field and produces 0 bytes delivered.
  - Drain chunks from `ChunkSource.Chunks()` into the response body until the request context is cancelled.
  - `Content-Type: audio/wav`.
- Unit test: inject a fake `audio.ChunkSource` with pre-filled chunks; assert WAV header is well-formed and chunks follow.

### Verification

Inside the container:

```bash
curl "http://localhost:8080/stream?spotify_uri=spotify:album:<id>" | ffplay -f wav -
# audio plays

# verify non-silence
curl -s "http://localhost:8080/stream?spotify_uri=spotify:album:<id>" | \
  ffmpeg -f wav -i pipe:0 -af volumedetect -f null - 2>&1 | grep mean_volume
# expect ~ -38 dB, not -91 dB (digital silence)
```

---

## Phase 6 — `cmd/mock-teddycloud` + SSE listener

**Goal:** full baseline integration test without a real Toniebox or Teddycloud. This is the integration gate — Phases 7–8 start only after this phase passes.

### Tasks

**`cmd/mock-teddycloud`:**
- `GET /api/sse`: SSE stream, keep-alive, emits events when triggered by button clicks.
- `GET /`: minimal HTML page with four buttons:
  - **Place figurine** → SSE event figurine-placed (includes the URI from `--uri` flag).
  - **Lift figurine** → SSE event figurine-lifted.
  - **Right ear** → SSE event right-ear-slap.
  - **Left ear** → SSE event left-ear-slap.
- `--addr` flag (default `:9090`), `--uri` flag (Spotify URI for figurine-placed events).
- SSE event format must match real Teddycloud exactly so the listener works against both without a code change.

**`internal/sselistener`:**
- Connect to `$TEDDYCLOUD_URL/api/sse`.
- Parse events:
  - `figurine-placed`: extract URI from payload, send WebSocket `play` with URI to Soloist.
  - `figurine-lifted`: send WebSocket `pause`.
  - `right-ear-slap`: send WebSocket `skip_next`.
  - `left-ear-slap`: send WebSocket `skip_prev`.
- **Auto-reconnect** with backoff on drop or Teddycloud restart.
- Unit test: feed synthetic SSE lines; assert correct WebSocket commands are produced.

### Verification

```bash
# terminal 1: mock
go run ./cmd/mock-teddycloud --uri spotify:album:<id>

# terminal 2: shim pointing at mock (container or local)
TEDDYCLOUD_URL=http://localhost:9090 SOLOIST_API_KEY=... ./shim

# terminal 3: audio
curl "http://localhost:8080/stream?spotify_uri=spotify:album:<id>" | ffplay -f wav -

# browser: http://localhost:9090
# click each button → verify shim logs show correct WebSocket command
# click Lift → audio pauses in ffplay
# click Place → audio resumes
# click Right ear → track skips
```

---

## Phase 7 — Hot-swap

**Goal:** swapping figurines replaces the active stream with no shim restart.

### Tasks

- `internal/server`: one active stream slot protected by a mutex.
  - On new `/stream` request: cancel the previous stream's context, then start the new one.
  - Send WebSocket `play` with the new URI.
  - Recorder channel keeps running — monitor source is always open.
- Unit test: simulate two concurrent `/stream` requests; assert the first context is cancelled when the second arrives.

### Verification

```bash
# stream album A
curl "http://localhost:8080/stream?spotify_uri=spotify:album:<A>" | ffplay -f wav - &

# while playing, open album B
curl "http://localhost:8080/stream?spotify_uri=spotify:album:<B>" | ffplay -f wav -
# album A curl terminates, album B plays — no shim restart
```

---

## Phase 8 — Integration with real Teddycloud

**Goal:** end-to-end test with actual hardware.

Only start this phase once Phase 6 (mock-teddycloud baseline) passes completely.

### Tasks

- Point `TEDDYCLOUD_URL` at the real Teddycloud instance.
- Verify SSE event format matches what the listener expects. Adjust parsing if needed (no logic change expected — mock was built to match).
- Configure a figurine in Teddycloud: stream URL = `http://<shim>:8080/stream?spotify_uri=<URI>`.
- Test all four physical controls on a real Toniebox.
- Test figurine swap.

### Verification

- Place figurine → Spotify audio plays on Toniebox.
- Lift figurine → audio pauses.
- Right ear → next track.
- Left ear → previous track.
- Swap figurine → old stream stops, new album starts.
- `/healthz` returns `200` throughout.

---

## Phase 9 — Polish

**Goal:** production-ready codebase.

### Tasks

- Structured logging with `log/slog`. All components use consistent field names. `LOG_LEVEL` controls level.
- Linting: all `.golangci.yml` issues resolved.
- CI: GitHub Actions — `go build`, `go test`, `golangci-lint`.
- README: pairing instructions, env var reference, Makefile targets, architecture diagram.
- Resolve all `TODO`/`FIXME` markers from earlier phases.

---

## Dependency graph

```
Phase 1 (skeleton + Containerfile)
  └── Phase 2 (PulseAudio orchestration)
        └── Phase 3 (null sink + recorder)
              └── Phase 4 (Soloist orchestration)
                    └── Phase 5 (/stream endpoint)
                          └── Phase 6 (mock-teddycloud + SSE listener)  ← integration gate
                                └── Phase 7 (hot-swap)
                                      └── Phase 8 (real Teddycloud)
                                            └── Phase 9 (polish)

Phases 2–4 can overlap with cmd/mock-teddycloud scaffolding (no dependency).
```

---

## Configuration reference

All configuration via environment variables.

| Variable | Required | Default | Description |
|---|---|---|---|
| `SOLOIST_API_KEY` | Yes | — | Spotify Soloist API key. Treat as secret. |
| `TEDDYCLOUD_URL` | Yes | — | Teddycloud base URL, e.g. `http://teddycloud:80` |
| `LISTEN_ADDR` | No | `:8080` | Shim HTTP listen address |
| `SOLOIST_DATA_DIR` | No | `/data` | Soloist data + session directory. Mount PVC here. |
| `SOLOIST_CACHE_DIR` | No | `/cache` | Soloist cache directory |
| `SOLOIST_DEVICE_NAME` | No | `teddycloud-spotify-shim` | Spotify Connect device name |
| `SOLOIST_BIN` | No | auto | Explicit path to soloist binary. Skips download if set. |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |
