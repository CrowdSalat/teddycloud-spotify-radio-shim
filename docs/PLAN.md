# Implementation Plan

Phased action plan. Each phase produces a testable artifact — no phase starts until the previous one passes its acceptance criteria.

**Language:** Go  
**References:** [PRD](PRD.md) · [Architecture](ARCHITECTURE.md) · [ADRs](adr/)

---

## Phase 0 — Project Skeleton

**Goal:** Buildable Go module with container image.

**Deliverables:**
- `go.mod` with module name
- `cmd/shim/main.go` — minimal `main()` that prints version and exits
- `Makefile` with targets: `build`, `test`, `lint`, `container`
- `Dockerfile` — multi-stage: build go-librespot from source + build shim, copy both into distroless/static runtime
- `.gitignore`

**Acceptance criteria:**
```bash
go build ./cmd/shim                          # exits 0
go test ./...                                # exits 0 (no tests yet, but no errors)
podman build --platform linux/amd64 -t shim-test .  # exits 0
```

---

## Phase 1 — Subprocess Lifecycle

**Goal:** Shim spawns go-librespot as a child process, monitors health, restarts on crash. ([ADR-002](adr/002-container-pattern.md))

**Deliverables:**
- `internal/supervisor/` — manages go-librespot child process
  - Creates FIFO (`mkfifo /tmp/spotify.fifo`)
  - Generates `config.yml` for go-librespot (pipe backend, API server on localhost:3678, interactive auth)
  - Spawns go-librespot with correct flags
  - Polls `GET http://localhost:3678/` until `playback_ready: true`
  - On child exit: restart with exponential backoff (1s → 2s → 4s → cap 30s)
  - Forwards SIGTERM/SIGINT to child, then exits

**Acceptance criteria:**
```bash
# Start the shim (go-librespot binary must be in PATH or built in Phase 0)
./shim &
PID=$!

# 1. go-librespot starts and API is reachable
sleep 5
curl -sf http://localhost:3678/ | jq -e '.playback_ready'
# → true

# 2. Kill go-librespot, verify shim restarts it
CHILD=$(pgrep -P $PID go-librespot)
kill $CHILD
sleep 5
curl -sf http://localhost:3678/ | jq -e '.playback_ready'
# → true (new process)

# 3. Clean shutdown
kill $PID
wait $PID
# → exits 0, no orphan go-librespot
```

---

## Phase 2 — FIFO Reader + Channel Buffer

**Goal:** Reader goroutine consumes the FIFO, writes to a buffered channel, handles overflow with discard. ([ADR-005](adr/005-fifo-backpressure.md))

**Deliverables:**
- `internal/audio/reader.go` — FIFO reader goroutine
  - Opens FIFO read end (blocking)
  - Reads 8 KB chunks
  - Sends to `chan []byte` (capacity 32 = 256 KB)
  - On channel full: `select/default` → discard, increment counter
  - Exposes metrics: `BytesRead`, `BytesDiscarded`, `ChunksDiscarded`
- `internal/audio/reader_test.go` — unit tests using an `os.Pipe()` as a stand-in for the FIFO

**Acceptance criteria:**
```bash
go test ./internal/audio/ -v -run TestReader
# Tests:
#   TestReaderNormalFlow        — write 100 chunks to pipe, read all from channel, 0 discards
#   TestReaderSlowConsumer      — write 100 chunks, don't read channel, assert discards > 0
#   TestReaderPauseResume       — write 10 chunks, stop writing (simulating go-librespot pause),
#                                 assert reader blocks harmlessly on empty pipe, then resumes
#                                 when writing restarts
#   TestReaderPanicRecovery     — (if applicable) verify reader goroutine handles errors from pipe read
```

---

## Phase 3 — `/stream` Endpoint + PCM Delivery

**Goal:** HTTP endpoint that calls go-librespot to play a URI, then streams audio from the channel to the HTTP response.

**Deliverables:**
- `internal/server/stream.go` — `GET /stream?spotify_uri=<URI>` handler
  - Calls `POST http://localhost:3678/player/play {"uri": "<URI>"}`
  - Reads from the audio channel, writes to `http.ResponseWriter`
  - Sets appropriate `Content-Type` header (PCM format or transcoded — determined by Teddycloud testing spike below)
  - On handler exit: logs disconnect
- **Spike: Teddycloud audio format acceptance** — test what Content-Type / format Teddycloud actually accepts by manually serving a known audio file. Document findings.

**Acceptance criteria:**
```bash
# Start shim (with go-librespot running from Phase 1)
./shim &

# Request a stream (use a known Spotify URI — requires authenticated session)
curl -s -o /tmp/audio_sample.raw \
     -w "%{content_type}\n%{http_code}\n" \
     --max-time 10 \
     "http://localhost:8080/stream?spotify_uri=spotify:track:4PTG3Z6ehGkBFwjybzWkR8"

# 1. HTTP 200
# 2. Content-Type header is set
# 3. File is non-empty and contains audio data
test -s /tmp/audio_sample.raw          # non-empty
wc -c < /tmp/audio_sample.raw          # > 0 bytes

# 4. go-librespot status shows playing
curl -sf http://localhost:3678/status | jq -e '.paused == false'
```

---

## Phase 4 — Hot-Swap + Stream Serialization

**Goal:** New `/stream` request terminates the previous one. One active stream at a time. (PRD §3.1)

**Deliverables:**
- `internal/server/stream.go` — extend to track active stream context
  - On new request: cancel previous stream's context → old HTTP response closes
  - Wait for `will_play` event from go-librespot WebSocket before forwarding
  - Flush channel of stale data on track transition
- `internal/librespot/events.go` — WebSocket client to go-librespot `/events`
  - Subscribes to `will_play`, `paused`, `playing`, `stopped` events
  - Exposes typed event channel

**Acceptance criteria:**
```bash
# Start shim with authenticated session
./shim &

# Start first stream in background
curl -s -N -o /dev/null \
     "http://localhost:8080/stream?spotify_uri=spotify:track:4PTG3Z6ehGkBFwjybzWkR8" &
CURL1=$!
sleep 3

# Start second stream — should kill first
curl -s -o /tmp/swap_test.raw --max-time 10 \
     "http://localhost:8080/stream?spotify_uri=spotify:track:6rqhFgbbKwnb9MLmUQDhG6"

# 1. First curl exited (connection closed by server)
wait $CURL1 2>/dev/null  # should have exited

# 2. Second stream got data
test -s /tmp/swap_test.raw

# 3. go-librespot is playing the second track
curl -sf http://localhost:3678/status | jq -e '.track.uri == "spotify:track:6rqhFgbbKwnb9MLmUQDhG6"'
```

---

## Phase 5 — SSE Event Ingestion

**Goal:** Connect to Teddycloud's SSE endpoint, parse events into typed internal actions. ([ADR-003](adr/003-control-channel.md))

**Deliverables:**
- `internal/control/sse.go` — SSE client
  - Connects to `$TEDDYCLOUD_URL/api/sse`
  - Parses `text/event-stream` format
  - Maps event names to internal `Action` enum: `ActionPause`, `ActionResume`, `ActionNext`, `ActionPrev`
  - Reconnects with exponential backoff on disconnect
  - Implements `ControlChannel` interface
- `internal/control/sse_test.go` — unit tests with a mock SSE server

**Acceptance criteria:**
```bash
go test ./internal/control/ -v -run TestSSE
# Tests:
#   TestSSEParseKnockForward    — mock SSE sends KnockForward event → channel emits ActionNext
#   TestSSEParseKnockBackward   — mock SSE sends KnockBackward event → channel emits ActionPrev
#   TestSSEParseTagPlaced       — mock SSE sends tag-placed event → channel emits ActionResume
#   TestSSEParseTagRemoved      — mock SSE sends tag-removed event → channel emits ActionPause
#   TestSSEReconnect            — mock SSE drops connection → client reconnects within 5s
#   TestSSEIgnoreIrrelevant     — mock SSE sends charger/volume events → no actions emitted
```

---

## Phase 6 — Event→API Wiring

**Goal:** SSE events trigger go-librespot API calls. Full control loop. ([ADR-004](adr/004-playback-control.md))

**Deliverables:**
- `internal/control/dispatcher.go` — reads from `ControlChannel.Events()`, calls go-librespot API
  - `ActionPause` → `POST /player/pause`
  - `ActionResume` → `POST /player/resume`
  - `ActionNext` → `POST /player/next`
  - `ActionPrev` → `POST /player/prev`
- Integration test combining mock SSE + real go-librespot

**Acceptance criteria:**
```bash
# Start shim with mock Teddycloud SSE server (or real Teddycloud)
./shim &

# Start a stream
curl -s -N -o /dev/null \
     "http://localhost:8080/stream?spotify_uri=spotify:track:4PTG3Z6ehGkBFwjybzWkR8" &
sleep 3

# Simulate figurine lifted (send SSE event via mock, or trigger on real box)
# Then verify go-librespot is paused:
curl -sf http://localhost:3678/status | jq -e '.paused == true'

# Simulate figurine placed back
# Then verify go-librespot resumed:
curl -sf http://localhost:3678/status | jq -e '.paused == false'

# Simulate knock forward
# Then verify track changed:
curl -sf http://localhost:3678/status | jq -e '.track.uri != "spotify:track:4PTG3Z6ehGkBFwjybzWkR8"'
```

---

## Phase 7 — Auth Flow

**Goal:** `/login` endpoint proxies go-librespot's interactive OAuth. (PRD §2.3)

**Deliverables:**
- `internal/server/login.go` — `/login` handler
  - If go-librespot is already authenticated: returns status message
  - If not: redirects to or displays go-librespot's OAuth URL
  - Handles callback proxying if go-librespot binds to a random port

**Acceptance criteria:**
```bash
# Start shim WITHOUT prior auth (delete state.json)
./shim &

# Hit login endpoint
HTTP_CODE=$(curl -s -o /tmp/login_response.html -w "%{http_code}" http://localhost:8080/login)

# 1. Returns 200 or 302
echo $HTTP_CODE  # 200 or 302

# 2. Response contains a Spotify authorization URL
grep -q "accounts.spotify.com" /tmp/login_response.html

# 3. After completing OAuth flow (manual), session persists
curl -sf http://localhost:3678/status | jq -e '.username != null'
```

---

## Phase 8 — Container + OpenShift Deployment

**Goal:** Production container image and OpenShift manifests.

**Deliverables:**
- `Dockerfile` — finalized multi-stage build (go-librespot + shim into distroless)
- `deploy/deployment.yaml` — OpenShift Deployment with:
  - `replicas: 1`
  - PVC mount at `/config` for go-librespot session
  - Environment variables for Teddycloud URL
  - Liveness probe: `/healthz`
  - Readiness probe: `/readyz`
  - Resource requests/limits
- `deploy/service.yaml` — ClusterIP Service on port 8080
- `deploy/route.yaml` — OpenShift Route (for `/login` OAuth callback)

**Acceptance criteria:**
```bash
# Build multi-arch image
podman build --platform linux/amd64,linux/arm64 -t docker.io/<user>/teddycloud-spotify-radio-shim:latest .

# Validate manifests
oc apply --dry-run=server -f deploy/

# Deploy to OpenShift (integration test)
oc apply -f deploy/
oc wait --for=condition=ready pod -l app=spotify-radio-shim --timeout=120s

# Verify readiness
oc exec deploy/spotify-radio-shim -- curl -sf http://localhost:8080/readyz
```

---

## Phase Dependencies

```
Phase 0 ──▶ Phase 1 ──▶ Phase 2 ──▶ Phase 3 ──▶ Phase 4
                                                    │
                                        Phase 5 ────┤
                                                    │
                                        Phase 6 ◀───┘
                                           │
                                        Phase 7
                                           │
                                        Phase 8
```

Phases 5 (SSE) can be developed in parallel with Phases 3-4 (audio path) since they have no code dependency — only Phase 6 wires them together.
