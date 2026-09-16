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

## 1b. Follow-up measurement (2026-09-16): the consumer pace is the HTTP presentation

Re-measured on the deployed pod after the Phase 3b.2 non-blocking recorder fix
(shim image `v0.1.3`, pipeline telemetry `LOG_LEVEL=debug`), playing a Spotify
tonie. The drop persists, now measured on both sides:

- Shim telemetry (`pipeline: util`, 10 s intervals):
  `chunks_s≈17–21 dropped_s≈20–25 drop_ratio≈0.49–0.60 delivered_kB_s≈68–87
  streams=1` — the `/stream` connection is open and the recorder produces
  exactly real time (~43 chunks/s ⇒ 176400 B/s). The shim only ever *delivers*
  what ffmpeg pulls.
- teddycloud ffmpeg: `size=12491kB time=00:01:06.64 bitrate=1535.5kbits/s
  speed=0.475x` — a steady drain at ~47.5 % real time.
- Cross-check: `176400 B/s × 0.475 ≈ 83790 B/s ≈ 84 kB/s`
  (observed `delivered_kB_s≈68–87`). All three numbers agree to within jitter:
  `drop_ratio` is *entirely explained by* ffmpeg's read pace.
- Control: a **radio tonie** on the same box, same ffmpeg path, plays at
  `speed=1.11x` — teddycloud's engine and the box hardware have ample headroom.

### Revised root cause (2026-09-16)

The 0.47x is **not** a decode-cost problem (the ffmpeg chain is a pure s16le
48 kHz-stereo passthrough, no transcode) and **not** an upstream teddycloud bug
(radio content plays real-time through the identical code path). It is a
property of how the shim presents the stream: a raw WAV body written as
4096-byte payloads every ~23 ms over chunked HTTP with no `Content-Length`.
Tiny per-segment writes trip client-side delayed-ACK coalescing, halving the
effective segment rate (0.475x ≈ one packet every other ~23 ms cadence window),
while the box's own content arrives as larger, burstier mp3 blobs and streams
at 1.1x.

### Decisions

- **No teddycloud fork, no upstream PR.** There is no general capability missing;
  teddycloud+box play real-time when fed normal-bodied streams. Upstream "works
  for everyone" precisely because it is fed well-formed audio.
- **Fix belongs in the shim** (Phase 12): batch `/stream` writes into ≥16 KB
  segments (~4–8 chunks per flush) instead of per-chunk drips, so the client
  sees fewer, ACK-friendly segments. Keep frame alignment (chunk size is already
  a multiple of the s16le 48k-stereo frame) and keep the recorder non-blocking.

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