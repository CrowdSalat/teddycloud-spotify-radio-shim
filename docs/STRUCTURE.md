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

**Status: done**

**Goal:** the shim compiles, passes lint, serves `/healthz`, and runs inside a container.

### Tasks

- `go mod init github.com/crowdsalat/teddycloud-spotify-radio-shim`
- Package layout:
  ```
  cmd/shim/            main entry point — wires concrete types to interfaces
  cmd/mock-teddycloud/ mock SSE server (stubbed, implemented in Phase 5)
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

**Status: done**

**Goal:** the shim finds or downloads the Soloist binary and verifies it executes.

### Tasks

- `internal/soloist`: `BinaryManager` struct.
  - Resolution order: `SOLOIST_BIN` env var → `soloist` on `$PATH` → `$SOLOIST_DATA_DIR/bin/soloist`.
  - If not found: download from Spotify CDN using `net/http` for the detected architecture (`runtime.GOARCH` → `x86_64` / `arm64` / `arm32`). Place at `$SOLOIST_DATA_DIR/bin/soloist`, `chmod +x`.
  - Check binary age: if modification time older than 80 days, log a warning. On exit code `10` (expiry): delete binary so next startup re-downloads.
  - Smoke-test: run `soloist --version` (or equivalent) to confirm the binary executes. Fail fast if it does not.
- `/healthz`: `503 {"error":"soloist_missing"}` if binary cannot be found or downloaded.

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

## Phase 2b — Session check + auto-pairing gate

**Status: done**

**Goal:** the shim detects whether Soloist has a stored session and, when it does not, drives the one-time Spotify Connect pairing itself — no manual `soloist --pair` run or host-side step required.

### Tasks

- `internal/soloist`: `SessionChecker` — inspect `$SOLOIST_DATA_DIR/settings/Users` for a stored session. The exact filename is determined by running Soloist once with `--pair` and observing what it writes.
- When unpaired, the shim spawns Soloist in pair mode itself:
  ```
  soloist --device-name $SOLOIST_DEVICE_NAME --api-key $SOLOIST_API_KEY
          --data-dir $SOLOIST_DATA_DIR --cache-dir $SOLOIST_CACHE_DIR --pair
  ```
  The device name always comes from `$SOLOIST_DEVICE_NAME` — never a hardcoded value — so the device the operator selects in the Spotify app matches the name the shim advertises at runtime.
- Log the app-side action for the operator:
  ```
  No Soloist session found. Pairing now — open the Spotify app and
  select the device "<name>" from the device picker.
  ```
- `/healthz` returns `503 {"error":"soloist_unpaired"}` while the session is missing. Do not crash-loop.
- Pairing lifecycle: `--pair` advertises a Connect device and keeps running until pairing completes, then stores the session and exits `0`. The shim waits for it and transitions to Phase 2c as soon as exit code `0` and `settings/Users` are observed. Non-zero exit (`1`) means pairing failed (e.g. invalid `$SOLOIST_API_KEY`) — log the reason and retry with exponential backoff. Exit `10` means the binary is expired — delete it (next startup re-downloads) and set state `expired`; do not retry pairing.
- Only one Soloist instance runs at a time: pair mode during Phase 2b, Connect mode during Phase 2c.
- Unit test: fake `ProcessManager` with pair exit `0` + session file present → state `ready`, no pair re-spawn. Pair exit `1` → retry scheduled with backoff. Missing session file → state `unpaired`; present session file → state `ready`.

### Verification

Pairing requires device discovery on the LAN — the container must run with `--network host` (as the Makefile targets do).

```bash
# run without a session directory
podman run --rm -p 8080:8080 --network host \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  -v ./container/soloist-data:/data:Z \
  shim:dev
curl -s http://localhost:8080/healthz   # → 503 {"error":"soloist_unpaired"}
# shim log: "Pairing now — open the Spotify app and select the device <name>"

# in the Spotify app: pick the advertised device from the device picker
curl -s http://localhost:8080/healthz   # → 200 OK (transition is automatic, no restart)
# shim log: "soloist paired, session stored"
```

---

## Phase 2c — Soloist subprocess lifecycle + WebSocket

**Status: done**

**Goal:** the shim spawns Soloist in Connect mode, holds an open WebSocket, and handles all exit conditions.

> Start only after the Phase 2b pairing gate has a stored session. Connect-mode spawning below owns the runtime subprocess; the pair-mode subprocess of Phase 2b is a different, one-time invocation.

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

**Status: done**

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
    `--daemonize=yes` is required: a foreground daemon stalls for ~10 s on unreachable D-Bus lookups in the trixie base image before binding the socket. Daemonize detaches the daemon from the spawned launcher, so crash detection cannot use `Wait()` — use the PID-file probe below.
  - Poll for the Unix socket at `$XDG_RUNTIME_DIR/pulse/native` until it appears (timeout: 10 s).
  - Verify sink exists: `pactl list sinks short` must show `virtual_out`.
  - Set default sink: `pactl set-default-sink virtual_out` — ensures Soloist routes audio to the virtual sink without explicit targeting.
- `Stop()`: send `pactl exit`, wait for the daemon to disappear (PID-file probe, grace ~3 s), then SIGTERM/SIGKILL the PID from `$XDG_RUNTIME_DIR/pulse/pid` as fallback.
- Crash recovery: the daemon is detached, so probe its PID file (`$XDG_RUNTIME_DIR/pulse/pid` + `/proc/<pid>/comm`) every ~2 s. When the probe fails, restart with exponential backoff. Remove the stale `native` socket + `pid` files before each respawn so the restart binds cleanly. Surface the failure via `/healthz` while restarting.
- `/healthz`: `503 {"error":"pulseaudio_not_ready"}` if PulseAudio failed to start or has crashed and is restarting.
- Unit tests:
  - Fake `ProcessManager` daemonizer exits non-zero → `Ready()` is false, error surfaced.
  - Probe starts passing after startup then fails → `Ready()` transitions to false, crash recovery restarts, backoff increases.
  - `Stop()` called → `pactl exit` is issued and the daemon is gone.

### Verification

Inside the container:

```bash
# pactl/parec ship in pulseaudio-utils — the pulseaudio package alone does not
# include them; make sure the Containerfile installs both.
podman run --rm -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  shim:dev
# shim log: "pulseaudio: ready"
curl -s http://localhost:8080/healthz   # → 200 OK
# podman exec does not inherit the shim's os.Setenv values — prefix the
# pactl calls with XDG_RUNTIME_DIR=/tmp/runtime.
podman exec <ctr> sh -c 'XDG_RUNTIME_DIR=/tmp/runtime pactl info'    # → Default Sink: virtual_out
podman exec <ctr> sh -c 'XDG_RUNTIME_DIR=/tmp/runtime pactl list sinks short'  # → virtual_out present

# crash recovery: kill PulseAudio, shim should notice and restart it
podman exec <ctr> sh -c 'kill -9 $(cat /tmp/runtime/pulse/pid)'
sleep 3
curl -s http://localhost:8080/healthz           # → 503 {"error":"pulseaudio_not_ready"} (while restarting)
sleep 5
curl -s http://localhost:8080/healthz           # → 200 OK (recovered)
podman exec <ctr> sh -c 'XDG_RUNTIME_DIR=/tmp/runtime pactl list sinks short'  # → virtual_out present again
# shim log: "pulseaudio: daemon not reachable, restarting (attempt N)"
```

---

## Phase 3b — Recorder

**Status: done**

**Goal:** the shim reads PCM bytes from `virtual_out.monitor`. Silence is fine — no Soloist playing yet.

Split into three independently verifiable tasks:

### 3b.1 — Core recorder logic

- Implement `ChunkSource` with a `PulseRecorder` concrete type using `github.com/jfreymuth/pulse` (pure Go, no cgo).
  - Format: `s16le`, 44100 Hz, stereo. Record from `virtual_out.monitor`.
  - `Chunks()` returns a buffered `chan []byte`.
  - **Backpressure safety valve:** discard chunks when channel is full. Must be a **non-blocking drop** (`select { default: }`) on the pulse library's connection goroutine — a blocking send fills the native-protocol socket queues and stalls the whole connection, commands included. Copy the inbound slice (the library writer may reuse its buffer).
  - Coalesce into frame-aligned fixed-size chunks (4 bytes/frame for s16le stereo).
  - Runs continuously, independent of any HTTP client and of Soloist availability.
  - `Stop()` shuts the pump down; the pump goroutine selects on context cancellation.
- Unit tests (no daemon needed — inject a fake producer):
  - Fake producer feeds frames → chunks arrive via `Chunks()`.
  - Full-channel discard does not block and does not stall the producer.
  - Coalescing produces fixed, frame-aligned chunk sizes.
  - `Stop()` terminates the pump.

### 3b.2 — Live PulseAudio integration

- Wire the recorder into `cmd/shim/main.go`, gated on PulseAudio readiness (retry until `pa.Ready()`, bounded). Independent of the Soloist branch — a missing/unpaired Soloist must not stop PCM from flowing.
- Set `PULSE_SERVER=unix:<XDG_RUNTIME_DIR>/pulse/native` in `SetupEnv()` so the library, `pactl`, and Soloist all resolve the same socket.
- Pin the null sink sample spec (`rate=44100 channels=2` in `daemonArgs`) so the recorder format matches the sink exactly instead of relying on client-side resampling.
- Verify lib API names against the real package (the research-doc `SinkByID` snippet is suspect — likely `SinkList` + name lookup).

> **Fallback note:** if `github.com/jfreymuth/pulse` proves insufficient in-container, implement `ParecRecorder` — runs `parec --device=virtual_out.monitor --format=s16le --rate=44100 --channels=2` via `StdoutPipe()`. `parec` ships in `pulseaudio-utils` (already installed), keeping ffmpeg out of the production image. Satisfies `ChunkSource`. Swap in `cmd/shim/main.go`. An `FFmpegRecorder` stays deferred until Phase 4+ needs resampling/DSP downstream.

### 3b.3 — Connectivity resilience

- Reconnect when the daemon dies: a daemon crash (see Phase 3a recovery) tears down the recorder's connection; after the restart a new sink/monitor exists but the old stream is dead. Detect connection loss and reconnect against `virtual_out` with exponential backoff — keyed off the same probe/restart events where possible.
- Startup must be safe against the daemon not being up yet.
- Optional: log a periodic chunk/drop summary at debug level so a silenced recorder is distinguishable from digital silence.

### Verification

Inside the container:

```bash
# shim log: "recorder started, reading from virtual_out.monitor"
# a live recording stream proves the Go recorder is connected and flowing
podman exec <ctr> sh -c 'XDG_RUNTIME_DIR=/tmp/runtime pactl list source-outputs short' | grep virtual_out
podman exec <ctr> parec --device=virtual_out.monitor --format=s16le | head -c 1024 | wc -c
# → 1024 (bytes flowing — silence is fine at this stage)
#
# 3b.2 note: the parec cross-check validates the monitor/daemon side — the Go
# recorder's liveliness is proven by the active source-output above.
```

Crash-resilience check (3b.3):

```bash
podman exec <ctr> sh -c 'kill -9 $(cat /tmp/runtime/pulse/pid)'
# shim log: "pulseaudio: daemon not reachable, restarting (attempt N)"
# shim log (recorder): reconnected to virtual_out.monitor
# debug chunk counter resumes incrementing
```

---

## Phase 4 — `/stream` endpoint

**Status: done**

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

**Status: done**

**Goal:** full baseline integration test without a real Toniebox or Teddycloud. This is the integration gate — Phases 6–7 start only after this phase passes.

### Tasks

**`cmd/mock-teddycloud`:**
- `GET /api/sse`: SSE stream, keep-alive, emits events when triggered by button clicks.
- `GET /`: minimal HTML page with buttons:
  - **Place figurine** → SSE event figurine-placed (includes the URI from `--uri` flag).
  - **Lift figurine** → SSE event figurine-lifted.
  - **Skip next / Skip back** → SSE event knock-forward / knock-backward.
  - **Ear pinch up / down** → SSE event ear-big / ear-small.
- `--addr` flag (default `:9090`), `--uri` flag (Spotify URI for figurine-placed events).
- SSE event format must match real Teddycloud exactly so the listener works against both without a code change.

**`internal/sselistener`:**
- Connect to `$TEDDYCLOUD_URL/api/sse`.
- Parse events:
  - `figurine-placed`: extract URI from payload, send WebSocket `play` with URI to Soloist.
  - `figurine-lifted`: send WebSocket `pause`.
  - `knock-forward`: send WebSocket `skip_next`.
  - `knock-backward`: send WebSocket `skip_prev`.
  - `pressed` (ear pinch): ignored — see the correction below.

> **Corrected 2026-09-27 (hardware, VLS1).** This phase originally mapped
> `right-ear-slap` → `skip_next` and `left-ear-slap` → `skip_prev`. That was a
> wrong assumption: the ears are the box's own **volume** control, and tapping
> the side of the box (`knock` `forward`/`backward`) is the **skip** gesture. An
> ear pinch made the track jump. The mapping is now knock → skip, ears ignored.
> Details and evidence: [research/teddycloud-sse-events.md](research/teddycloud-sse-events.md).

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
# click Skip next → track skips
# click Ear pinch → nothing (box volume only)
```

---

## Phase 6 — Integration with real Teddycloud

**Status: done** — live-discovered event formats captured and fixes applied (2026-09-09).

**Goal:** validate the SSE event format and the control mapping against the real Teddycloud and a real Toniebox, and **fix `cmd/mock-teddycloud` so the mock matches reality**. This is the point where assumed event formats get verified — the mock becomes trustworthy for the phases that follow.

Event formats were **discovered live** on 2026-09-09 — see [research/teddycloud-sse-events.md](research/teddycloud-sse-events.md) (raw capture: `research/teddycloud-sse-capture.txt`). They are the authoritative source for the mock fix. Notable: the real server has **no `TagInvalid`** (a figurine lift surfaces as `playback` `stopped`), and ears emit `pressed` + `ear-big`/`ear-small`. The listener may need a small change: map `playback` + `stopped` → pause; that is accepted here so Phase 7's development does not repeat the mismatch.

### Tasks

- **Fix the mock:** update `cmd/mock-teddycloud` so its SSE event payloads match the real server **byte-for-byte** per `research/teddycloud-sse-events.md` — `TagValid` (tonie UID, not URI), `playback` `starting/started/stopped`, `pressed` `ear-big`/`ear-small`, plus the `VolumeLevel`/`VolumedB` volume pairs and ~16 s keep-alive. The mock stays the standing dev harness for Phase 7.

### Verification

```bash
# SSE reachable without auth via port-forward (oauth-proxy sidecar bypassed)
oc port-forward svc/teddycloud 8080:80 -n app-teddycloud
curl -s http://localhost:8080/api/sse   # → heartbeats; event payloads on box action

# mock now matches the real server byte-for-byte
# trigger a figurine place on the real box, then on the mock (no --uri:
# TagValid carries the tonie UID, not a Spotify URI)
diff <(curl -s http://localhost:8080/api/sse) \
     <(go run ./cmd/mock-teddycloud)
# → equivalent event payloads for TagValid / playback / pressed (+ keep-alive), line for line
```

- Place figurine → Spotify audio plays on a real Toniebox.
- Lift figurine → audio pauses.
- Tap the right side of the box → next track.
- Tap the left side of the box → previous track.
- Pinch either ear → volume changes on the box, **the track does not change**.
- `/healthz` returns `200` throughout.

---

## Phase 7 — Hot-swap

**Status: done** — implemented and verified against the mock; real-hardware figurine swap is part of Phase 11 final acceptance.

**Goal:** swapping figurines replaces the active stream with no shim restart. Developed and verified against the Phase 6-validated mock — no Toniebox required.

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

Figurine swap on real hardware is re-verified in the Phase 11 final acceptance.

---

## Phase 8 — Container image in GitHub Container Registry

**Status: implemented** — 8.1/8.2/8.3/8.4 done. The GHCR package is **public**; tag-driven publishing works.

**Goal:** the shim image is built and pushed to `ghcr.io/crowdsalat/teddycloud-spotify-radio-shim` as a **public** image. Soloist is not baked in — it is downloaded at runtime, so the redistribution concern holds for a public image too, and the API key is a runtime env var.

Split into independently verifiable subtasks. The push itself is driven by the 8.2 CI workflow using the auto-provisioned `GITHUB_TOKEN` — no manual PAT required.

### 8.1 — Makefile targets

- `Makefile`: add `container-push-ghcr` target.
  - Login: `podman login ghcr.io` (uses `GITHUB_TOKEN` or `gh auth token`).
  - Build multi-arch manifest (amd64 primary, arm64 secondary):
    ```
    podman build --platform linux/amd64,linux/arm64 \
      --manifest ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest \
      -f Containerfile .
    ```
  - Push:
    ```
    podman manifest push --all \
      ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest \
      docker://ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
    ```
  - Clean up local manifest: `podman manifest rm ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest`.
- `Makefile`: add `container-tag` target for versioned tags (e.g. `ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:v0.1.0`).

### 8.2 — CI workflow (GHCR push)

- GitHub Actions workflow covering the Go checks and the GHCR build/push. Push uses the automatically provisioned `GITHUB_TOKEN` (`permissions: contents: read, packages: write`) — no manual token or secret.
  - Checks-only job on push (all branches) and PRs: `go build ./...`, `go test ./...`, `golangci-lint run ./...` (amd64 host runner is fine — no cross-compile needed for CI signals).
  - Build+push job triggered only by **version tags** matching `v*` (semver `vMAJOR.MINOR.PATCH`, pre-releases like `v0.2.0-rc.1` allowed):
    - Version = the git tag itself (`$GITHUB_REF_NAME` → `vX.Y.Z`). Release by `git tag vX.Y.Z && git push origin vX.Y.Z`. **Version tags are immutable — never delete or overwrite a published one.**
    - Publishes `ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:vX.Y.Z` (immutable release artifact) and moves `:latest` to it (convenience pointer, not a version; not updated on main pushes).
    - Multi-arch via `docker/setup-buildx-action` + `docker/build-push-action` (QEMU binfmt): `linux/amd64` (primary) + `linux/arm64`.
    - Tag build gates on the Go checks passing first.
  - v0 bump policy (Go module convention): MINOR for features / breaking changes (`v0.1.0` → `v0.2.0`), PATCH for bugfixes (`v0.1.0` → `v0.1.1`).
  - Moved here from the former Phase 10.3 so the image-publishing pipeline is owned by the phase that needs it.

#### Verification

- Pushed workflow run is green on a feature branch before merging (checks-only job).
- Pushing a `v*` tag lands `:vX.Y.Z` (+ `:latest` pointer) in GHCR; `podman pull ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:vX.Y.Z` succeeds with no registry login.

### 8.3 — Package visibility + pull docs

- Repository settings: the GHCR package visibility is **Public** (Settings → Packages → teddycloud-spotify-radio-shim → Visibility → Public). An unauthenticated pull of the manifest returns `200`, which is the check that proves it.
- Soloist is downloaded at container start, never at build time, so nothing redistributable ends up in a layer; `SOLOIST_API_KEY` is an env var supplied by the caller. Public visibility is therefore safe.
- `README.md`: document how to pull the image — a plain `podman pull`, no login step:
  ```bash
  podman pull ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
  ```

### 8.4 — Changelog generation (git-cliff)

- Generate the release changelog from commits with [git-cliff](https://git-cliff.org) — the repo already follows Conventional Commits, so messages parse as-is.
  - `cliff.toml` — default conventional template: per-version sections grouped by type (Features, Bug Fixes, Performance, Documentation, Refactor, Others), scoped to the git range since the previous tag.
  - `Makefile`: `changelog` target — regenerate the committed `CHANGELOG.md` (`git-cliff -o CHANGELOG.md`). Requires `git-cliff` installed.
  - Release flow (ties into 8.2): before tagging a release — run `make changelog`, commit `CHANGELOG.md` with the release commit, then `git tag vX.Y.Z && git push origin vX.Y.Z` (the tag triggers the 8.2 publish job; version = the tag). git-cliff groups the commits since the previous tag; it does not choose the version.
  - The version bump stays a manual choice per the 8.2 v0 policy — git-cliff's `--next-version` is a suggestion only, never applied automatically.
  - `CHANGELOG.md` is a committed artifact; it is not generated at CI time.
  - Optional: the 8.2 publish job may embed the new section into the GitHub Release body.

#### Verification

```bash
make changelog
git diff CHANGELOG.md       # → new unreleased section, conventional grouping (feat/fix/docs/refactor)
# release: commit changelog → git tag vX.Y.Z → push tag (CI publishes the image)
```

### Verification

```bash
# push
make container-push-ghcr
# → "Pushed: docker.io/ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest"

# verify public (unauthenticated manifest read succeeds)
podman manifest inspect docker://ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
# → the image index is printed (public)

# pull, no auth
podman pull ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
podman run --rm \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:latest
curl -s http://localhost:8080/healthz   # → 200 OK or 503 (soloist_missing expected — no session yet)
```

---

## Phase 9 — Session migration script

**Status: implemented** — `scripts/migrate-session.sh` + `migrate-session` make target; pending verification against an OpenShift pod/PVC.

**Goal:** a shell script copies the paired Soloist session from the local `container/soloist-data/` into the OpenShift PVC `soloist-session-data`, deleting any stale session for the same Spotify **user** first.

### Context

The paired session lives in `container/soloist-data/settings/Users/<user-id>-user/`. The `<user-id>` is the Spotify account the session was paired with. On OpenShift, the PVC is mounted at `/data` (the `SOLOIST_DATA_DIR` default). If a session for the same user already exists on the PVC, it must be replaced — the old token is stale and re-pairing requires the device to match.

The binary (`bin/soloist`) does not need to be migrated — it re-downloads at runtime if expired. The `cache/` directory is optional (can be ephemeral).

### Tasks

- `scripts/migrate-session.sh`:
  - **Required argument:** `SOURCE_DIR` — path to local `container/soloist-data/` (or equivalent).
  - **Required argument:** `TARGET_POD` — OpenShift pod name (or use `oc cp` directly to PVC via a temp pod).
  - Determines the user ID from `SOURCE_DIR/settings/Users/` — there must be exactly one `<user-id>-user/` directory. Fail if none or more than one; fail if `settings/Users` is missing.
  - Lists existing user directories in the target at `/data/settings/Users/`. If a directory with the **same `<user-id>-user`** name exists, delete it first (old session for the same user). Other users' sessions are left untouched.
  - Copies the following from `SOURCE_DIR` to `/data/`:
    - `.device_id`
    - `settings/` (entire tree — includes `Users/<user-id>-user/` and `prefs`)
  - Does **not** copy `bin/` (binary), `cache/` (ephemeral), `crashpad/` (debug), lock/pid files.
  - Prints a summary: user ID, device ID, files copied, old session deleted (if any).
  - Dry-run mode: `--dry-run` flag — prints what would happen without modifying the target.
- `Makefile`: add `migrate-session` target:
  ```makefile
  migrate-session:
  	./scripts/migrate-session.sh $(SOURCE_DIR) $(TARGET_POD)
  ```
  With `SOURCE_DIR ?= $(CURDIR)/container/soloist-data/` and `TARGET_POD` as a required override.

### Session structure reference

```
container/soloist-data/
├── .device_id                  ← device identity (UUID)
├── settings/
│   ├── prefs                   ← device preferences
│   └── Users/
│       └── <user-id>-user/     ← Spotify Connect pairing session
│           ├── offline2        ← session token (critical)
│           ├── prefs
│           ├── offline_abp
│           ├── offline_ep
│           ├── offline_media
│           ├── ad-state-storage.bnk
│           └── offline_lists.bnk
├── bin/soloist                 ← binary (do NOT migrate)
├── cache/                      ← ephemeral (do NOT migrate)
├── crashpad/                   ← debug (do NOT migrate)
├── soloist.pid                 ← runtime (do NOT migrate)
├── ws.port                     ← runtime (do NOT migrate)
└── .lock                       ← runtime (do NOT migrate)
```

### Verification

```bash
# dry run — no changes on target
./scripts/migrate-session.sh container/soloist-data/ my-shim-pod --dry-run
# → "Would delete old session for user 31q2zwxalia2nc5hdgo4ldwydwvm-user on target"
# → "Would copy settings/Users/31q2zwx...-user/ (6 files)"
# → "Would copy .device_id, settings/prefs"

# actual migration
./scripts/migrate-session.sh container/soloist-data/ my-shim-pod
# → "Deleted old session for user 31q2zwx...-user"
# → "Copied session: user=31q2zwx... device=411af102-... files=8"

# verify on target
oc exec my-shim-pod -- cat /data/.device_id
# → 411af102-a2a0-4adb-96fa-b1d46439cfd4
oc exec my-shim-pod -- ls /data/settings/Users/
# → 31q2zwxalia2nc5hdgo4ldwydwvm-user/

# re-run is idempotent — old session for same user is replaced
./scripts/migrate-session.sh container/soloist-data/ my-shim-pod
# → "Deleted old session for user 31q2zwx...-user"
# → "Copied session: user=31q2zwx... device=411af102-... files=8"

# different user — old sessions preserved, new one added
oc exec my-shim-pod -- ls /data/settings/Users/
# → 31q2zwx...-user/  <other-user>-user/
```

---

## Phase 10 — Polish

**Goal:** production-ready codebase.

Split into independently verifiable tasks. Every subtask must keep `golangci-lint run ./...` at 0 issues — linting is a gate, not a deliverable.

### 10.1 — Backoff refactor

- Consolidate the duplicated exponential-backoff helpers — two identical `NextBackoff` (`internal/audio/pulse.go`, `internal/soloist/pair.go`), three identical ctx-aware `sleep` variants (`pulse.go`, `supervisor.go`, `cmd/shim/main.go` `recorderSleep`), and the duplicated `defaultStartBackoff`/`defaultMaxBackoff` (5 s/60 s) — into a new `internal/backoff` package:
  ```
  internal/backoff/backoff.go       backoff.Next(current, max), backoff.Sleep(ctx, d)
  internal/backoff/backoff_test.go  single table test (union of both TestNextBackoff)
  ```
- `Next` doubles `current`, clamped at `max`, overflow-safe; `Sleep` returns `false` when ctx is cancelled. `internal/backoff` imports only `context`/`time`, so audio and soloist can both import it without a cycle.
- Delete the three `sleep` copies, both `NextBackoff` copies, and both `TestNextBackoff` tables; swap the ~13 call sites.
- Keep per-consumer tuning consts (`pairingBackoffInitial/Max`, `recorderBackoffInitial/Max`) and the struct getters (`startBackoff()`/`maxBackoff()`) where they are — they are consumer-specific, not shared defaults.

#### Verification

```bash
gofmt -w internal/backoff internal/audio/pulse.go internal/soloist/pair.go internal/soloist/supervisor.go cmd/shim/main.go
go build ./... && go vet ./... && go test ./... -count=1 && golangci-lint run ./...
rg "NextBackoff|\bsleep\(" internal cmd | grep -v "_test.go"
# → hitless except internal/backoff; behaviour identical to pre-refactor backoffs
```

### 10.2 — Structured logging + marker cleanup

- Structured logging audit across all components: consistent slog field names (e.g. `err`, `attempt`, `backoff`), one component prefix per subsystem, no `fmt.Println`/`log` leftovers. `LOG_LEVEL` (debug/info/warn/error) controls the level.
- Resolve all `TODO`/`FIXME` markers from earlier phases.

#### Verification

```bash
# run shim with LOG_LEVEL=warn → no info/debug lines; with LOG_LEVEL=debug → recorder live summary
rg -n "TODO|FIXME|fmt\.Print(l|f)?n?\(|log\.[A-Z]" --glob '*.go' --glob '!**/*_test.go'
# → no output
```

### 10.3 — README

- Pairing instructions, env var reference (see Configuration reference below), Makefile targets, architecture diagram.

#### Verification

- README covers the four sections; every env var from the configuration reference appears in the README table; diagram matches DESIGN.md.

### 10.4 — Comment audit

Phase 6's live discovery changed the SSE reality (real Teddycloud emits `TagValid`/`playback`, no `TagInvalid`; ears are `pressed` `ear-big`/`ear-small`), and later phases may drift further. Doc comments must describe what the code actually does, not what an old design assumed.

- Walk every package doc comment (`Package <name>` headers), struct/field docs, and protocol comments (SSE events, Soloist WS commands) in `internal/` and `cmd/`.
- Cross-check each against the current code and the discovery in `docs/research/teddycloud-sse-events.md` — flag stale event names (`figurine-placed`, `figurine-lifted`, `TagInvalid`, `right-ear-slap`, ...), wrong keep-alive intervals, and superseded mapping notes.
- Fix comments to match the implemented behaviour, not the other way around. Do not change behaviour in this subtask.
- Note: this is a pointed pass over the whole codebase once phases 6–7 have settled the event/URI mapping — do not fold it into 10.2's marker cleanup.

#### Verification

```bash
rg -i "figurine-placed|figurine-lifted|right-ear-slap|left-ear-slap|TagInvalid" --glob '*.go' docs/
# → no stale event-name references in code comments or docs (except DESIGN.md, which keeps the abstract names)
rg -n "keep-alive|keepalive" --glob '*.go'   # → interval matches the mock's 16 s
go build ./... && golangci-lint run ./...
```

---

## Phase 11 — OpenShift manifests

**Status: implemented** — `container/ocp/` Kustomize app + `deploy-ocp` make target; pending apply and final acceptance on OpenShift.

**Goal:** the shim can be deployed on OpenShift from declarative manifests in `container/ocp/`: PVC `soloist-session-data` mounted at `/data`, public GHCR image pull, Secret-fed `SOLOIST_API_KEY`, probes wired to `/healthz`. Requires the Phase 8 image and the Phase 9 migration script.

> **Investigate:** the current `restricted-v3` SCC solution feels overly complicated. We set `hostUsers: false` at the pod-`spec` level (a field `securityContext` silently prunes because it does not exist there) plus `fsGroup: 1000` because `restricted-v3`'s `MustRunAs` range rejects GID 0. `runAsNonRoot`/`seccompProfile` would be auto-mutated by admission, and OpenShift would allocate `fsGroup` from the range anyway. Worth investigating whether a minimal spec (just `hostUsers: false` + no `securityContext` block, letting admission fill the rest) validates, and whether the PVC remains writable under user namespaces without an explicit stable `fsGroup`.

### Tasks

- Manifest files in `container/ocp/` (apply via `oc apply -k container/ocp/`, or `Makefile` `deploy-ocp` target):
  - `namespace.yaml` — target namespace.
  - `pvc.yaml` — `soloist-session-data` PVC, `ReadWriteOnce`, 1 Gi, mounted at `/data` (`SOLOIST_DATA_DIR`).
  - `secret.yaml` — placeholder Secret template for `SOLOIST_API_KEY` (value via `oc create secret` or SealedSecret/ExternalSecret — never in git or the image). Deployment references it via `secretKeyRef`.
  - `deployment.yaml` — runs the `ghcr.io/crowdsalat/teddycloud-spotify-radio-shim` image:
    - No `imagePullSecrets`: the GHCR package is public, so the kubelet pulls it anonymously.
    - env: `TEDDYCLOUD_URL`, `SOLOIST_DEVICE_NAME`, `LOG_LEVEL`; `SOLOIST_API_KEY` via `secretKeyRef`.
    - `volumeMounts`: PVC `soloist-session-data` at `/data`; `emptyDir` cache at `/cache` (`SOLOIST_CACHE_DIR`).
    - `securityContext` per OpenShift `restricted-v2` SCC (image already `USER 65534:0`); run as the assigned random UID, no privilege escalation.
    - liveness/readiness probes on `/healthz`.
  - `service.yaml` — `ClusterIP` Service exposing `LISTEN_ADDR` (default `:8080`).
  - `kustomization.yaml` — resources list + `generate` the Secret `stringData` placeholder (or document `oc create secret generic soloist-api-key`).
- `Makefile`: `deploy-ocp` target — `oc apply -k container/ocp/`.
- **Final acceptance:** re-run the Phase 6 hardware tests against the OCP deployment — all four physical controls **plus figurine swap** (hot-swap, Phase 7). The swap depends on the OCP pod's `/stream` handling and on Teddycloud reaching the shim's Service (use the Service DNS or expose a Route to the shim for this test).

### Verification

```bash
# scrub local session into the PVC via the script, then deploy
./scripts/migrate-session.sh container/soloist-data/ my-shim-pod

oc apply -k container/ocp/
oc get pod -l app=teddycloud-spotify-shim
# → Running, Ready 1/1
oc get pvc soloist-session-data
# → Bound

# pod uses the migrated session, no re-pairing required
oc logs deployment/teddycloud-spotify-shim | grep -i soloist
# → "soloist ready, WebSocket connected" (no "Pairing now" — session restored)
curl -s http://<route-or-svc>:8080/healthz   # → 200 OK
```

Final acceptance on the deployed pod:

- Place figurine → Spotify audio plays on Toniebox.
- Lift figurine → audio pauses.
- Tap the right side of the box → next track.
- Tap the left side of the box → previous track.
- Pinch either ear → volume only, no track change.
- Swap figurine → old stream stops, new album starts (hot-swap over the SDN, `TEDDYCLOUD_URL=http://teddycloud:80`).
- `/healthz` returns `200` throughout.

---

## Phase 12 — Playback quality: stop recorder audio drops (jumps)

**Status: done** — fixed 2026-09-18 (see
[research/streaming-pipeline-e2e.md](research/streaming-pipeline-e2e.md) §4b).

**Goal:** a raw-WAV `/stream` that teddycloud's ffmpeg can drain at real time,
so the recorder's drop-on-full safety valve stops dropping audio.

### Context

Recorder produces exactly real time (~21.5 chunks/s = 88200 B/s ÷ 4096 B at
22050 Hz). teddycloud's ffmpeg drained at ~0.47x and the 8-chunk (~186 ms)
internal buffer filled, so the non-blocking `default:` branch discarded ~half
the audio. The 2026-09-16 measurement (pipeline telemetry + radio control)
shows the 0.47x is a *client-side artifact of the tiny per-chunk HTTP writes*
(delayed-ACK segment coalescing), not an upstream deficiency: the same box plays
radio at 1.11x via the identical ffmpeg code path. The drop must stay off the
pulse library's connection goroutine (a blocking send there stalls the
native-protocol socket queues and wedges the whole connection — the original
Phase 3b.1 constraint), but the consumer's read pace must become real time.

### Resolution (v0.1.9)

1. **Bigger buffer** — `defaultBufferLen` raised to 256 chunks (≈12 s,
   `RECORDER_BUFFER` env override) so transient consumer stalls are absorbed
   instead of dropping.
2. **Flush stale pre-fill at connect** — `PulseRecorder.Flush()` + the
   `flushStalePreFill` call in `handleStream` snap each `/stream` consumer to
   the live edge. Without this, a connect inherited up to a buffer's worth of
   pre-connect audio and dumped it as an instant burst (ffmpeg at 95× then
   `Encoding aborted` → silent box).

### Verification

```bash
# on the OCP pod: recorder counters, deliver rate tracks 88200 B/s real time
oc logs -l app=teddycloud-spotify-shim -n app-teddycloud | grep -E "pipeline:|chunks"
# → dropped ≈ 0 and delivered ≈ 84.4 kB/s while playing; clean audio on the Toniebox
# → teddycloud ffmpeg speed ≈ 1.0x on a Spotify tonie, no "Encoding aborted"

# local: the stream writes occur in batched segments, not per chunk
go test ./internal/server/... -run TestStreamCounters -v
```

---

## Phase 13 — Playback quality: Soloist volume 100 at runtime

**Status: done** — implemented (2026-09-15).

**Goal:** playback is not at ~40 % volume because Soloist restores its persisted volume at startup and the shim never sets it.

### Context

`playback_state` reports `"volume":40`. PulseAudio sink/monitor are at 100 %, unmuted — the attenuation is Soloist's own persisted volume. teddycloud's `VolumeLevel`/`VolumedB` are box-local and correct to ignore. Fix on the Soloist control path: `set_volume 100` after `activate`, and/or `-i/--initial-volume 100` at spawn.

### Tasks

- Add a volume command to `internal/soloist/connector.go` (with Play/Pause/SkipNext/SkipPrev): send `{"type":"command","command":"set_volume","volume":100}` (range 0–100, see research/spotify-soloist.md).
- Call it after `activate` (in `Supervisor` or connector activation) so every Soloist start self-heals to 100 regardless of persisted state.
- Optionally add `-i/--initial-volume 100` to `Supervisor.args()` as a second safety net, and/or a `SOLOIST_VOLUME` env (default `100`) for operator control.
- Verify a `playback_state` `volume` of 100 appears in `/healthz`-adjacent status / logs after reconnect.
- Keep `VolumeLevel`/`VolumedB` SSE ignored — they are box speaker level, not Soloist gain.

### Verification

```bash
# OCP pod reconnect: volume self-heals without manual command
oc logs deployment/teddycloud-spotify-shim | grep -E "volume|activate"
# → set_volume 100 sent after activate; playback_state volume == 100
# Toniebox audio no longer ~40 % amplitude; mean_volume rises toward -3..-6 dB
```

---

## Phase 14 — Fail fast on unwritable data/cache dirs

**Goal:** a missing or read-only mount is reported as an actionable configuration
error *before* the shim spends a full Soloist download discovering it.

### Context

`BinaryManager.Resolve()` resolves in the order `SOLOIST_BIN` → `$PATH` →
`$SOLOIST_DATA_DIR/bin/soloist` → download. The download branch does `http.Get`
(`internal/soloist/binary.go:93`) and only *then* `os.MkdirAll($SOLOIST_DATA_DIR/bin)`
(`internal/soloist/binary.go:104`), so an unwritable `/data` is discovered after
the whole tarball is fetched, and surfaces as `soloist binary unavailable`
(`cmd/shim/main.go:150`) — a message that points at Spotify's CDN or the network
rather than at the volume spec.

The shim does not exit there: it sets `/healthz` to `503 soloist_missing` and keeps
serving, so a misconfigured deployment sits unhealthily instead of failing fast. With
the OCP liveness probe (`initialDelaySeconds: 120`, `failureThreshold: 10`,
`periodSeconds: 30`) the pod only restarts after ~7 minutes.

The image deliberately ships **no** `/data` or `/cache` (see the comment in the
`Containerfile`): both deployment paths create them outside the image — the homelab
playbook pre-creates the host dirs `0770` owned by `65534:0`, and OpenShift creates
the mountpoints and applies `fsGroup`. A mount masks whatever the image ships, so
adding the dirs would not fix the deployed cases; it would only convert a forgotten
mount into silent writes to the container's writable layer, which `--rm` and the OCP
`Recreate` strategy then discard together with the paired device session. **Keep the
image free of writable dirs — the fix belongs in the startup path, not the image.**

### Tasks

- In `cmd/shim/main.go`, validate **both** `$SOLOIST_DATA_DIR` and `$SOLOIST_CACHE_DIR`
  *before* `bm.Resolve()`, so no network work happens on a misconfigured deployment.
  - `os.Stat` the dir: missing → "not mounted" wording. Then a write probe
    (`os.CreateTemp` in the dir + remove): present but failing → "not writable" wording.
  - Do **not** `MkdirAll` the roots. Creating them reintroduces the silent-writable-
    layer hazard above; every supported deployment pre-creates them.
- New `/healthz` reasons `data_dir_unwritable` / `cache_dir_unwritable` instead of
  reusing `soloist_missing` — the reason string is what the operator reads, and
  `soloist_missing` misdirects them to the CDN.
- Keep the no-exit convention from Phase 2b (503, do not crash-loop): the instant
  ERROR log is the fail-fast part, and a hard exit would fight the liveness probe.
- Log at ERROR, naming the path, the uid/gid, and the remedy:
  ```
  SOLOIST_DATA_DIR /data is not mounted or not writable (uid=65534 gid=0);
  mount a volume there — "podman run -v /var/lib/apps/tc-spotify-shim:/data:Z"
  or a PVC on OpenShift
  ```
- Unit tests: missing dir → `data_dir_unwritable`; `/data` present but `/cache`
  missing → `cache_dir_unwritable` (the partial-mount case that must not pass
  silently); both present → `Resolve` is reached, no regression. Keep the
  not-writable branch uid-independent (a `0o555` dir is still writable for root,
  and tests may run as root) by injecting the probe as a func.

### Verification

```bash
# no volume → fails immediately, no CDN download
podman run --rm --network none -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  localhost/teddycloud-spotify-radio-shim:dev
# log: "data_dir_unwritable" naming /data and the -v remedy
# log: NO "soloist binary: downloading" line — the probe precedes any network work
curl -s http://localhost:8080/healthz   # → 503 {"error":"data_dir_unwritable"}

# partial mount: /data only, /cache forgotten → caught, not silently tolerated
podman run --rm --network host -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  -v /tmp/shim-data:/data:Z \
  localhost/teddycloud-spotify-radio-shim:dev
# log: "cache_dir_unwritable" — otherwise the cache is lost on every restart

# both mounted → behaviour unchanged (falls through to download/pair)
podman run --rm --network host -p 8080:8080 \
  -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  -v /tmp/shim-data:/data:Z -v /tmp/shim-cache:/cache:Z \
  localhost/teddycloud-spotify-radio-shim:dev

go test ./... -count=1 && golangci-lint run ./...
```

---

## Phase 15 — Retire the legacy `teddycloud-spotify-shim` package

**Status: planned** — not started. The repository no longer references the legacy
image anywhere (Phase 8 was rewritten for the new name); the package itself still
exists in GHCR and is the last remaining artifact of the old name.

**Goal:** `ghcr.io/crowdsalat/teddycloud-spotify-shim` stops being a reachable
image — either because every reference is gone, or because the package is deleted —
and the deletion is *decided* rather than discovered: every consumer is known, every
tag that must keep working has a replacement, and the failure modes are written down
before the registry is touched.

### Context

The rename from `teddycloud-spotify-shim` to `teddycloud-spotify-radio-shim` was
cosmetic in content but created a **new package** in the registry, because GHCR
packages are keyed by name. The old name is a separate repository with its own tags,
its own access rules, and consumers this repository cannot see.

Measured state of both packages (`skopeo list-tags`, unauthenticated):

| Package | Tags | Notes |
|---|---|---|
| `…/teddycloud-spotify-shim` (legacy) | `v0.1.0`, `v0.1.3`, `v0.1.4`, `v0.1.5`, `v0.1.6`, `v0.1.7` | no `v0.1.2`. `v0.1.0` and `v0.1.3`–`v0.1.6` are multi-arch indexes; **`v0.1.7` is a single amd64 manifest** — the last release before the workflow was fixed. Readable anonymously, i.e. **publicly pullable today**. |
| `…/teddycloud-spotify-radio-shim` (current) | `0.1.9`, `0.1.10`, `latest`, plus `7-hex` SHA and `-amd64`/`-arm64` tags | multi-arch. Tags carry **no `v` prefix** — the workflow publishes `type=semver,pattern={{version}}` — and `latest` is only set by pushes to the default branch, not by tag pushes. |

Why that needs a phase instead of a `docker rmi`:

- **The release history is split.** `0.1.0`–`0.1.7` exist only under the old name and
  there is no `0.1.8` in either package. A consumer pinned to
  `…spotify-shim:v0.1.7` has no drop-in replacement: the fix is to move it to a
  *current* tag, because `…radio-shim:v0.1.7` does not exist and never will.
- **"Who still uses it" is not answerable from our side.** The legacy package is
  publicly pullable, so any host that pulled it before the rename may still run it,
  and any `docker://` reference in someone else's CI job, `Dockerfile`, Helm/ArgoCD
  value, or runbook keeps resolving silently until the package disappears.
- **Local caches mask the breakage.** A host holding the old image in its Podman/OCI
  cache starts `Recreate`d containers just fine after deletion — including a rollback
  that was never actually tested. Verification must test a *cold* pull.
- **Deletion is not recoverable.** GHCR has no undelete and no per-tag delete in the
  UI. The source tags `v0.1.0`, `v0.1.3`–`v0.1.7` are in this repository's history, so
  the images are *reconstructible*, but a rebuild is not guaranteed bit-identical to
  what was released.
- **It cannot be deleted from this environment.** The available token carries
  `admin:public_key, gist, read:org, repo` — no `read:packages`, no
  `delete:packages`; `gh api …/packages/container/teddycloud-spotify-shim` answers
  `403: You need at least read:packages scope`, and deletion additionally requires
  package *admin*. This is an org-owner action in the UI (or with a fresh PAT).

### Tasks

1. **Decide the outcome first: keep, mirror, or delete.** Deleting is not required to
   stop *new* deployments from picking up the old name — the repository no longer
   references it. If task 2 finds a live consumer, finish task 3 and stop; if it finds
   none, go through tasks 4–7. Record the decision in this section when it is made.
2. **Inventory consumers before touching the registry.** This is the step that decides
   whether deletion is safe, so write down what was checked *and what was not*:
   - Repository: `rg -n 'ghcr\.io/crowdsalat/teddycloud-spotify-shim'` over all tracked
     files. The remaining bare `teddycloud-spotify-shim` strings in
     `internal/config/config.go` (the `SOLOIST_DEVICE_NAME` default),
     `docs/research/ocp-playback-issues.md`, and the `oc get` examples are the
     **device/resource name, not the image** — they stay.
   - Deployments: VLS1 (`../homelab` → `app_image:`, already on
     `…radio-shim:0.1.10`) and the live OpenShift deployment —
     `oc get deploy -n app-teddycloud -o jsonpath='{.items[*].spec.template.spec.containers[*].image}'`
     reports what the cluster is really running, independent of the manifests here.
   - Local caches: `podman images | grep teddycloud-spotify-shim` and
     `podman ps -a --filter ancestor=ghcr.io/crowdsalat/teddycloud-spotify-shim:v0.1.7`
     on VLS1 and every dev host.
   - GitHub: workflow files (already `$GITHUB_REPOSITORY`-derived), Dependabot config,
     release notes, and `docker://` references in issues/PRs. The `CHANGELOG.md` entry
     "Rename image to teddycloud-spotify-shim" is a historical record — keep it, and
     do not rewrite git history as part of this phase.
3. **Mirror any tag that must keep working**, before it can be lost:
   ```bash
   skopeo copy --all docker://ghcr.io/crowdsalat/teddycloud-spotify-shim:v0.1.5 \
     docker://ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:0.1.5
   skopeo inspect --raw docker://ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:0.1.5 | sha256sum
   ```
   Repeat per tag, and pin the **digest** in anything that must not drift. A mirror
   inherits the arch asymmetry: mirroring `v0.1.7` gives an amd64-only tag, which does
   not help an arm64 host.
4. **Delete the legacy package** (human action, org-owner credentials):
   - UI: org → `Settings` → `Packages` → `teddycloud-spotify-shim` → `Delete package`
     (the full name must be typed to confirm), or
   - `gh api -X DELETE /orgs/crowdsalat/packages/container/teddycloud-spotify-shim`
     with a token holding `delete:packages` **and** package admin.
   All tags go at once. There is no undelete.
5. **Fix the Phase 8 tag-form contradiction found while doing this.** Phase 8 states
   that a `v*` push publishes `:vX.Y.Z` and moves `:latest` to it; the workflow
   actually publishes `:X.Y.Z` and sets `:latest` only on default-branch pushes. Every
   released tag is `0.1.x`, so the Phase 8 text, the `container-tag` example, and the
   8.2 verification are all wrong. Pick one convention (publish the git tag verbatim
   with `type=ref,event=tag`, or correct the docs to `:X.Y.Z`) and make workflow,
   Makefile, and docs agree — do not leave the docs describing tags that do not exist.
6. **Optional local cleanup, strictly after task 4:** drop the cached legacy image on
   the hosts found in task 2 (`podman rmi …`) so no host can keep serving a deleted
   image. Doing this earlier spends the only rollback path that silently exists.

### Consequences and rollback

| Action | What breaks | Recovery |
|---|---|---|
| Delete while a host is still pinned to `…spotify-shim:v0.1.x` | `podman pull` fails; on OpenShift, `ImagePullBackOff`. Invisible while the host serves from cache. | Repoint the deployment to a current `…radio-shim` tag. If the old version is genuinely required, rebuild it from its source tag and push it under the new name (task 3). |
| Delete before mirroring a tag still in use | No path back through the old name, and the exact released digests are gone. | Rebuild from `git tag v0.1.x` (all tags are in this repo) and re-push; close to, but not guaranteed identical to, the original layers. |
| Delete while a third party is mid-pull | Transient failures for external consumers, outside our control — unavoidable given the package is public. | None locally; content is reproducible from the source tags. |
| Prune local caches too early | The rollback path that never existed in the registry is used up. | Same as any other pull failure: repoint to a current tag. |

### Verification

```bash
# 1. no live reference to the legacy image in the repository
rg -n 'ghcr\.io/crowdsalat/teddycloud-spotify-shim' .   # → only this phase, or nothing

# 2. cold registry check: a deleted package 404s for anonymous and authenticated
#    callers alike, while the current package is untouched
skopeo list-tags docker://ghcr.io/crowdsalat/teddycloud-spotify-shim
# → "name unknown" / 404  → the legacy package is gone
skopeo list-tags docker://ghcr.io/crowdsalat/teddycloud-spotify-radio-shim
# → still lists 0.1.x + latest  → the current package survived

# 3. the current image still pulls multi-arch and runs
podman pull --platform linux/amd64,linux/arm64 \
  ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:0.1.10
podman run --rm -e SOLOIST_API_KEY=test -e TEDDYCLOUD_URL=http://localhost \
  ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:0.1.10
curl -s http://localhost:8080/healthz   # → 200 OK or 503 (soloist_missing — no session yet)

# 4. consumers, cold: a redeploy must resolve the image, not serve it from cache
oc rollout restart -n app-teddycloud <deployment>   # the one from task 2
oc rollout status  -n app-teddycloud <deployment>
# VLS1: remove the container (image only) → redeploy → no pull error, /healthz ok
```

---

## Dependency graph

```
Phase 1  (skeleton + Containerfile)
  └── Phase 2a (Soloist binary management)
        └── Phase 2b (session check + pairing gate)
              └── Phase 2c (subprocess lifecycle + WebSocket)
└── Phase 3a (PulseAudio daemon + virtual sink)
                            └── Phase 3b (recorder: core logic → live PulseAudio → resilience)
                                  └── Phase 4  (/stream endpoint)
                                      └── Phase 5  (mock-teddycloud + SSE listener)  ← integration gate
                                            └── Phase 6  (real Teddycloud — validate & fix mock)
                                                  └── Phase 7  (hot-swap, against validated mock)
                                                        └── Phase 8  (GHCR public image)
                                                              └── Phase 9  (session migration script)
                                                                    └── Phase 10 (polish)
                                                                          └── Phase 11 (OpenShift manifests)

Phase 9 (session migration script) — independent, can run any time after Phase 2b has a paired session.
Phase 11 (OpenShift manifests) — needs Phase 8 image + Phase 9 migration script.
                                    └── final acceptance: Phase 6 hardware tests + Phase 7 figurine swap on the deployed pod

Phase 12 (stop recorder drops) — needs deployed OCP pod (Phase 11) for reproduction; recorder changes must respect the Phase 3b.1 pulse-goroutine constraint.
Phase 13 (volume 100) — needs deployed OCP pod (Phase 11) for verification; independent of Phase 12.
Phase 14 (fail fast on unwritable dirs) — independent of every phase; local Go change, verifiable with `podman run` alone.
Phase 15 (retire legacy GHCR package) — needs Phase 8; deletion is irreversible, so the consumer inventory is the gate, not a code change.

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
| `SOLOIST_DATA_DIR` | No | `/data` | Soloist data + session directory. Mount PVC here. Must exist and be writable — the shim never writes into an unmounted path (Phase 14). |
| `SOLOIST_CACHE_DIR` | No | `/cache` | Soloist cache directory. Must exist and be writable (Phase 14). |
| `SOLOIST_DEVICE_NAME` | No | `teddycloud-spotify-shim` | Spotify Connect device name |
| `SOLOIST_VOLUME` | No | `100` | Soloist playback volume (0–100). Sent via `set_volume` after activate and as `--initial-volume` at spawn. |
| `SOLOIST_BIN` | No | auto | Explicit path to soloist binary. Skips download if set. |
| `LOG_LEVEL` | No | `info` | `debug`, `info`, `warn`, `error` |
