# Research: OCP playback quality issues (2026-09-14)

Observed on the deployed OpenShift pod (`teddycloud-spotify-shim`) while playing
"Die drei ??? — Falsche Schuld" on the real Toniebox, 2026-09-14.

Two distinct defects, separate root causes:

1. **Jumps / gaps** — recorder drop-on-full discards ~50 % of chunks when the
   consumer reads slower than real time.
2. **Very low volume** — Soloist plays at its persisted volume `40/100`; the shim
   never sets volume.

---

## 1. Jumps: recorder drop-on-full

### Evidence

- Soloist-side health is fine: playback_state events all report
  `status: playing` (Inhaltsangabe, Titelmusik, Kapitel 01).
- Recorder counters on the live pod: `chunks` grows ~22/s, `dropped` grows ~21/s
  → **~50 % of chunks are thrown away**, producing audible holes/jumps.
- Total production (chunks + dropped) ≈ 43 chunks/s ≈ exactly real time
  (176400 B/s ÷ 4096 B/chunk) — the recorder captures fine; the consumer is the bottleneck.
- The consumer is teddycloud's ffmpeg:
  `ffmpeg -i "http://teddycloud-spotify-shim:8080/stream?spotify_uri=..." -f s16le -acodec pcm_s16le -ar 48000 -ac 2 -ss 0 -`
  running at `speed=0.47x` — it decodes/resamples slower than real time.

### Root cause

`PulseRecorder.pump` uses **drop-on-full** (the Phase 3b.1 "backpressure safety
valve"): a non-blocking `select { default: }` that discards a chunk whenever the
internal channel is full (`defaultBufferLen = 8` chunks ≈ 186 ms buffer). This is
the correct choice on the *pulse library connection goroutine* — a blocking send
there would stall the native-protocol socket queues and wedge the whole
connection — but **it trades away audio integrity**: any consumer that drains
slower than real time silently loses audio. ffmpeg in teddycloud qualifies
(0.47x), so ~half the stream vanishes.

### Fix direction

Do not block on the pulse connection goroutine. Instead, decouple: move the
chunk channel out of the pulse goroutine and let the HTTP `/stream` handler (or a
dedicated pump goroutine) own the queue. When the queue is full and the consumer
is behind, either (a) send the chunk to a second, larger buffer before any drop,
or (b) deliberately **reset the stream** (new WAV header + `play` re-issue) and
tell the consumer to resync rather than emitting a mangled stream. A larger
`BufferLen` alone only stretches the ~186 ms cushion; it does not remove the
drop.

Recorded as Phase 12 task.

---

## 2. Very low volume: Soloist persisted volume 40

### Evidence

- `playback_state` reports `"volume":40` on the live pod.
- PulseAudio sink topology is at correct level: `pactl` shows `virtual_out` and
  `virtual_out.monitor` at 100 %, unmuted (checked with
  `HOME=/data XDG_RUNTIME_DIR=/tmp/runtime pactl ...`).
- Therefore the attenuation is not the audio daemon — it is **Soloist's own
  volume**, because Soloist restores its *persisted* volume (40/100) at startup.

### Root cause

- `internal/soloist/supervisor.go` builds the Soloist command line without
  `-i/--initial-volume 100`, so Soloist starts at whatever it last stored.
- The shim never sends a `set_volume` command after `activate`
  (`internal/soloist/connector.go` has Play/Pause/SkipNext/SkipPrev only).
- Teddycloud's `VolumeLevel`/`VolumedB` SSE events are box-local (speaker level)
  and correctly debug-ignored — they cannot be used to set Soloist volume.

### Fix direction

Pick one:

- **On activation:** after WS `activate`, send
  `{ "type":"command", "command":"set_volume", "volume":100 }` (0–100 range
  documented). Self-healing even if Soloist restarts with a stale persisted
  volume.
- **At spawn:** add `-i/--initial-volume 100` to `Supervisor.args()`.
  Single-point-of-truth but does not recover if volume is changed mid-run.

Recommended: volume 100 on activate, plus optionally honour a
`SOLOIST_VOLUME`/`TEDDYCLOUD_VOLUME` env default.

Recorded as Phase 13 task.