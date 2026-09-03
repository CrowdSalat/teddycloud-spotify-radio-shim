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

## Phase 2a — Soloist binary management

**Goal:** the shim finds or downloads the Soloist binary and verifies it executes.

### Tasks

- `internal/soloist`: `BinaryManager` struct.
  - Resolution order: `SOLOIST_BIN` env var → `soloist` on `$PATH` → `$SOLOIST_DATA_DIR/bin/soloist`.
  - If not found: download from Spotify CDN using `net/http` for the detected architecture (`runtime.GOARCH` → `x86_64` / `arm64` / `arm32`). Place at `$SOLOIST_DATA_DIR/bin/soloist`, `chmod +x`.
  - Check binary age: if modification time older than 80 days, log a warning. On exit code `10` (expiry): delete binary so next startup re-downloads.
  - Smoke-test: run `soloist --version` (or equivalent) to confirm the binary executes. Fail fast if it does not.
- `/healthz`: `503 {"error":"soloist_missing"}` if binary cannot be found or downloaded.
- Unit test: fake filesystem + fake HTTP server returning a dummy tarball; assert binary is placed at the correct path.

### Verification

```bash
# without a binary present — shim should download it
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  -v ./container/soloist-data:/data:Z \
  shim:dev
# shim log: "soloist binary downloaded to /data/bin/soloist"
# shim log: "soloist version: ..."
curl -s http://localhost:8080/healthz   # → not soloist_missing
```

---

## Phase 2b — Session check + pairing gate

**Goal:** the shim detects whether Soloist has a stored session and surfaces a clear operator error if not.

### Tasks

- `internal/soloist`: `SessionChecker` — inspect `$SOLOIST_DATA_DIR` for a session file. The exact filename is determined by running Soloist once with `--pair` and observing what it writes.
- If no session found: log a clear operator message:
  ```
  No Soloist session found. Run pairing once:
    soloist --device-name <name> --api-key <key> --data-dir /data --pair
  Then select the device in the Spotify app.
  ```
- Surface via `/healthz`: `503 {"error":"soloist_unpaired"}`. Do not crash-loop. Wait and re-check on an interval (30 s) so the operator can pair without restarting the container.
- Unit test: missing session file → state is `unpaired`; present session file → state is `ready`.

### Verification

```bash
# run without a session directory
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  shim:dev
curl -s http://localhost:8080/healthz   # → 503 {"error":"soloist_unpaired"}
# shim log: clear pairing instruction

# after pairing, restart — healthz should return 200
```

---

## Phase 2c — Soloist subprocess lifecycle + WebSocket

**Goal:** the shim spawns Soloist in Connect mode, holds an open WebSocket, and handles all exit conditions.

### Tasks

- `internal/soloist`: `Supervisor` struct using `ProcessManager`.
  - Spawn Soloist:
    ```
    soloist --device-name $SOLOIST_DEVICE_NAME --api-key $SOLOIST_API_KEY
            --data-dir $SOLOIST_DATA_DIR --cache-dir $SOLOIST_CACHE_DIR
            --ws 127.0.0.1:0
    ```
  - Poll `$SOLOIST_DATA_DIR/ws.port` until it appears (timeout: 15 s).
  - Open WebSocket to `ws://127.0.0.1:<ws.port>`. Send `activate` once connected.
  - Read and discard incoming WebSocket events (keeps connection healthy; `auth_state` loss detected here in future).
  - Exit-code handling:
    - `0`: unexpected — log warning, restart with exponential backoff.
    - `1`: log error, restart with backoff.
    - `10`: log expiry, delete binary, **do not restart** — set state to `expired`.
    - Signal/crash: restart with exponential backoff.
- `/healthz`: `503 {"error":"soloist_expired"}` when state is `expired`.
- Unit test: fake `ProcessManager` emits exit code `10` → state is `expired`, no restart attempted. Emits exit code `1` → restart is attempted after backoff.

### Verification

Inside the container (requires paired session from Phase 2b):

```bash
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY -e TEDDYCLOUD_URL=http://localhost \
  -v ./container/soloist-data:/data:Z \
  shim:dev
# shim log: "soloist ready, WebSocket connected on port <n>"
curl -s http://localhost:8080/healthz   # → 200 OK
```

---

## Phase 3a — PulseAudio daemon + virtual sink

**Goal:** the shim starts PulseAudio and the virtual sink is available. No recorder yet.

### Tasks

- Implement the `AudioDaemon` interface with a `PulseAudio` concrete type wrapping a `ProcessManager`.
  - Before spawning: set `HOME` and `XDG_RUNTIME_DIR` to writable scratch dirs (`/tmp/home`, `/tmp/runtime`) via `os.Setenv`. Avoids "Failed to create secure directory" when running with `--userns=keep-id`.
  - Start PulseAudio with the null sink loaded in the same invocation:
    ```
    pulseaudio --exit-idle-time=-1 -n
      --load=module-native-protocol-unix
      --load=module-null-sink sink_name=virtual_out sink_properties=device.description=Shim_Sink
      --daemonize=yes --log-target=stderr
    ```
    `module-native-protocol-unix` must be loaded explicitly with `-n` — without it no client socket is created and all clients fail with "Connection refused".
  - Poll for the Unix socket at `$XDG_RUNTIME_DIR/pulse/native` until it appears (timeout: 10 s).
  - Verify sink exists: `pactl list sinks short` must show `virtual_out`.
- `/healthz`: `503` with reason if PulseAudio failed to start.
- Unit test: fake `ProcessManager` exits immediately → `Ready()` is false, error surfaced.

### Verification

Inside the container:

```bash
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  shim:dev
# shim log: "PulseAudio ready"
curl -s http://localhost:8080/healthz   # → 200 OK
podman exec <ctr> pactl info
podman exec <ctr> pactl list sinks short   # → virtual_out present
```

---

## Phase 3b — Recorder

**Goal:** the shim reads PCM bytes from `virtual_out.monitor`. Silence is fine — no Soloist playing yet.

### Tasks

- Implement `ChunkSource` with a `PulseRecorder` concrete type using `github.com/jfreymuth/pulse` (pure Go, no cgo).
  - Format: `s16le`, 44100 Hz, stereo. Record from `virtual_out.monitor`.
  - `Chunks()` returns a buffered `chan []byte`.
  - **Backpressure safety valve:** discard chunks when channel is full — prevents PulseAudio client buffer from stalling.
  - Runs continuously, independent of any HTTP client.
- Unit test: fake chunk reader → chunks arrive via `Chunks()`, full-channel discard does not block.

> **Fallback note:** if `github.com/jfreymuth/pulse` proves insufficient, implement `FFmpegRecorder` — runs `ffmpeg -f pulse -i virtual_out.monitor -f s16le -ar 44100 -ac 2 pipe:1` via `StdoutPipe()`. Satisfies `ChunkSource`. Swap in `cmd/shim/main.go`. Nothing else changes.

### Verification

Inside the container:

```bash
# shim log: "recorder started, reading from virtual_out.monitor"
podman exec <ctr> parec --device=virtual_out.monitor --format=s16le | head -c 1024 | wc -c
# → 1024 (bytes flowing — silence is fine at this stage)
```

---

## Phase 4 — `/stream` endpoint

**Goal:** `curl /stream | ffplay` plays Spotify audio end-to-end.

### Tasks

- `internal/server`: register `GET /stream`. Handler depends on the `ChunkSource` interface — never imports the concrete recorder package directly.
  - Parse `?spotify_uri=` query param. Return `400` if missing or malformed.
  - Send WebSocket `play` command with the URI to Soloist.
  - Write **streaming WAV header**: RIFF and data size fields both `0xFFFFFFFF`. Do **not** compute `0xFFFFFFFF + 36` — overflows the 32-bit field, delivers 0 bytes.
  - Drain chunks from `ChunkSource.Chunks()` into the response body until request context is cancelled.
  - `Content-Type: audio/wav`.
- Unit test: fake `ChunkSource` with pre-filled chunks → WAV header is well-formed, chunks follow.

### Verification

Inside the container (requires paired Soloist session):

```bash
curl "http://localhost:8080/stream?spotify_uri=spotify:album:<id>" | ffplay -f wav -
# audio plays

curl -s "http://localhost:8080/stream?spotify_uri=spotify:album:<id>" | \
  ffmpeg -f wav -i pipe:0 -af volumedetect -f null - 2>&1 | grep mean_volume
# expect ~ -38 dB, not -91 dB (digital silence)
```

---

## Phase 5 — `cmd/mock-teddycloud` + SSE listener

**Goal:** full baseline integration test without a real Toniebox or Teddycloud. This is the integration gate — Phases 6–7 start only after this phase passes.

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

## Phase 6 — Hot-swap

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

## Phase 7 — Integration with real Teddycloud

**Goal:** end-to-end test with actual hardware.

Only start this phase once Phase 5 (mock-teddycloud baseline) passes completely.

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

## Phase 8 — Polish

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
Phase 1  (skeleton + Containerfile)
  └── Phase 2a (Soloist binary management)
        └── Phase 2b (session check + pairing gate)
              └── Phase 2c (subprocess lifecycle + WebSocket)
                    └── Phase 3a (PulseAudio daemon + virtual sink)
                          └── Phase 3b (recorder)
                                └── Phase 4  (/stream endpoint)
                                      └── Phase 5  (mock-teddycloud + SSE listener)  ← integration gate
                                            └── Phase 6  (hot-swap)
                                                  └── Phase 7  (real Teddycloud)
                                                        └── Phase 8  (polish)

cmd/mock-teddycloud scaffolding can be started any time after Phase 1.
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
