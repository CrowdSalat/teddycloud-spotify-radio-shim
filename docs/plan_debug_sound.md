# Debug plan: sound issues on VLS1

Date: 2026-09-28
Deployed build: `ghcr.io/crowdsalat/teddycloud-spotify-radio-shim:0.1.10` (rev `4775d71`)
Box: `opc@130.61.85.57`, containers `tc-radio-shim` (LOG_LEVEL=debug, `TEDDYCLOUD_URL=http://teddycloud:80`),
`teddycloud` 0.7.0, `caddy`.

**Current focus: Issue 1.** Issues 2–4 are recorded but not being worked on now.

## Verified baseline (do not regress)

| Behaviour | State | Evidence |
|---|---|---|
| First placement plays | works | `07:06:12 sselistener: play` → `07:06:13 command_result play` |
| Audio is real-time, lossless | works | `pipeline: util ... delivered_kB_s=84.4 drop_ratio=0.00 streams=1`; teddycloud ffmpeg `time=00:03:04.27 speed=1.03x` |
| Ear pinch does not skip | **fixed and confirmed on hardware** | `07:09:05 ignoring pressed event (box volume) data={"type":"pressed","data":"ear-big"}`, `sselistener: skip_*` count = 0, one `track_changed` total |

84.4 kB/s is exactly 22050 Hz stereo s16 at 1.0x (88200 B/s), so the pipeline figure is a true
real-time measure. It does **not** distinguish music from digital silence — only the box's
speakers (or a measured RMS) can.

## Open issues

| # | Issue | Status |
|---|---|---|
| 1 | Re-placing a figurine produces silence; audio returns only ~10–20 s later | **in progress** |
| 2 | Pipe is content-blind: silence floods it, and drops can eat music | open, separate |
| 3 | Skipping gestures do nothing | open, separate |
| 4 | `position_sync` events stop permanently after the first track | open, observability only |

Issues 1 and 2 are unrelated and must not be fixed in the same commit: 1 is a control-path
problem (no URI, so nothing plays), 2 is a signal-path problem (the pipe is blind to what it is
carrying). They do share one input — the SSE playback state — which is why the intent gating in
Issue 2 is fed by the same events Issue 1 fixes.

---

# Issue 1 — Re-placement gives silence  *(focus)*

## Symptom

Lift a figurine, wait a few seconds, place it back → no sound. Audio returns only after
10–20 s of silence, apparently on its own.

## Evidence already collected (VLS1, 2026-09-28)

```
07:06:12.339  sselistener: play                                    ← TagValid
07:06:12.341  error: play requires a valid playable Spotify URI    ← Play("") rejected
07:06:13.223  command_result play                                   ← 0.9 s later: /stream arrived, play OK
07:09:07.434  sselistener: play                                    ← re-placement, TagValid
07:09:07.435  error: play requires a valid playable Spotify URI    ← Play("") rejected again
              (no command_result play — no new /stream reached the shim)
```

`context_changed` appears exactly **once** in the whole log (album `6At2D64cCtuwymOUmGy6DK`),
so the re-placement never changed the playing context. `sselistener: pause` never appears, even
though the figurine was lifted — so no `playback: stopped` was received, or it arrived and the
command failed silently.

## Solution

`TagValid` is the only reliable "a figurine is on the box" signal, and it carries no URI. So the
shim must stop treating it as "play something unknown" and start treating it as **resume whatever
is already loaded**.

1. **Cache the last valid URI in `Server`.** `handleStream` already receives `spotify_uri` on
   every request; store the last valid one (with its stream id) as the currently-loaded URI. No
   new plumbing, no config, no new interface.
2. **`TagValid` → `Play(cachedURI)`.** If the cache is empty (nothing has ever been streamed),
   send nothing at all. This is the fix for the re-placement silence: it no longer depends on
   teddycloud issuing a new `/stream` request, which is the thing that observably does not happen.
3. **Delete the `Play("")` path.** It is a guaranteed `play requires a valid playable Spotify URI`
   error on every placement and it has never accomplished anything.
4. **Do not clear the cache on `playback: stopped`.** That would reintroduce this exact bug on the
   next placement. The cache means "what is loaded", and lifting a figurine does not unload it.
5. **Do not branch on consumer state.** Always sending `play(cachedURI)` is safe and idempotent
   when the same track is already playing, and it avoids a fragile "is a consumer attached"
   condition. One code path beats a conditional one.

### Why this and not something else

- The capture shows a re-placement is a **normal, complete** playback cycle:
  `TagValid → playback starting → started → ContentAudioId → ContentTitle`, repeated identically on
  all four placements, with `ContentAudioId` fixed at `436906887`. The SSE side of a re-placement
  is indistinguishable from a first placement, and content identity is never the problem — the
  only thing missing is a URI on the shim side.
- A "wait for a fresh `/stream`" fix cannot work, because the whole failure is that the reconnect
  does not arrive.
- "Tear the stream down on lift so teddycloud must re-request" pushes the fix onto teddycloud's
  behaviour, which the shim cannot verify. Caching the URI works whether or not teddycloud ever
  re-requests.

### The one open question, and how to answer it

Does `play <currentTrackURI>` **resume in place** or **restart from 0:00**? This decides whether
the cache should hold the track URI or the album/context URI, and it is not answerable from the
capture (SSE only, no Soloist data). It is cheap to measure on the box: after a mid-track lift and
re-placement, read the `playback_state` event, which carries `"position":{"position_ms":…}` —
a position near where it stopped means resume, ≈0 means restart. Note this is independent of
Issue 4: `playback_state` arrives on every play, while `position_sync` is the thing that went quiet.

### One thing the log contradicts, which needs its own task

`sselistener: pause` **never appears** in the container's entire log, although the figurine was
lifted. So either `playback: stopped` never reached the shim, or the lift was too brief to trigger
it. Until that is settled the lift→pause path is unverified, and it may not even be the reason the
box goes quiet while the figurine is up — teddycloud stopping its own output would do that too.
Do not build on top of this path before it is measured.

### Diagnostics that must ship with the fix

The reason this bug survived a "healthy" pipeline is that `delivered_kB_s=84.4 drop_ratio=0.00`
is *exactly* what a stream of digital silence looks like. The metrics reported health while the box
was mute. So the fix is not complete without:

- `/stream` attach/detach logging (task 1.1) — otherwise the next occurrence is equally opaque.
- a non-silence measure on the pipeline (RMS or non-silent-sample fraction), so "bytes are
  flowing" and "audio is audible" stop being the same statement.

## Architecture facts that constrain the fix

- `TagValid` carries the tonie UID, **never a URI**. The only source of a playable URI is the
  `GET /stream?spotify_uri=…` query (`internal/server/stream.go:18`).
- `handleStream` cancels the previous stream, calls `s.play(uri)`, then streams audio
  (`internal/server/stream.go:47-88`).
- Therefore, if teddycloud does not open a *new* `/stream`, the shim never learns a URI and
  never plays. Today the shim keeps **no memory** of the last known URI.
- There is no `TagInvalid` event. A lift surfaces only as `playback: stopped` → `Pause()`.
- `Play("")` is a guaranteed error on every placement. It is pure noise.

## Ranked hypotheses

- **H1 (most likely).** On lift, teddycloud emits `playback: stopped` but keeps the existing
  `/stream` HTTP connection open. On re-placement teddycloud therefore does not re-request the
  stream, so the shim never gets a URI. The shim stays paused → silence. The 10–20 s recovery
  is *not* the re-placement succeeding; it is the next track/segment boundary making teddycloud
  issue a fresh `/stream`, which finally delivers a URI.
- **H2.** A new `/stream` does arrive, but `s.play(uri)` fails or is a no-op because soloist
  still holds the previous paused context/queue.
- **H3.** A new `/stream` arrives and `play` succeeds, but the audio path is wedged
  (stale pre-fill, or the `PulseRecorder` channel-closure bug at
  `internal/audio/recorder.go:152-195`) and delivers silence. Note this overlaps Issue 2 —
  if the classification lands on H3, Issue 1 is not closed until the buffer behaviour is settled.

H1 and H3 are distinguishable only once `/stream` attach/detach is observable — and right now
`handleStream` logs **nothing** on the success path. Instrument first.

## Tasks

- [ ] **1.1 Instrument the stream lifecycle (no behaviour change).** In
      `internal/server/stream.go`, log on every request:
      attach (`uri`, `stream_id`, whether a previous consumer was cancelled → hot-swap) and
      detach (`stream_id`). INFO level. Cover `handleStream` entry, the `s.streamCancel()` hot-swap
      branch, and the deferred teardown.
- [ ] **1.2 Deploy the instrumented build.** Rebuild, bump the image, deploy via
      `ansible-playbook site-tc-radio-shim.yml`, confirm the new lines appear.
- [ ] **1.3 Run the re-placement capture** (recipe below) for 5 lift/place cycles, 3–5 s apart.
      Record for each cycle: `TagValid` time, `pause` time, `/stream` attach time, `play`
      `command_result` time, `pipeline` `delivered_kB_s` and `streams`, and the audible-silence
      window the operator reports. This is the decisive dataset.
- [ ] **1.4 Classify against the hypotheses.** From 1.3:
      no `/stream` attach on re-place → **H1**; attach but play rejected → **H2**;
      attach + `command_result play` + `delivered_kB_s>0` yet inaudible → **H3**.
- [ ] **1.5 Settle the resume-vs-restart question experimentally** (the one open question
      above). Mid-track, lift and re-place the figurine, then read `playback_state.position_ms`:
      ≈ the pre-lift position → resume, ≈0 → restart. If it restarts, cache the context/album URI
      instead of the track URI.
- [ ] **1.6 Establish what actually happens on lift.** Confirm whether `playback: stopped` ever
      reaches the shim (it never has, per the log). If it does, verify the shim's `pause` and what
      silences the box. If it does not, establish what does — teddycloud stopping its own output
      is the likely answer — and record which mechanism is actually in play before touching the
      lift path.
- [ ] **1.7 Add a non-silence measure to the pipeline telemetry** (RMS or non-silent-sample
      fraction per interval). Without it, "healthy" and "mute" are indistinguishable in the logs,
      which is how this bug hid. Report it next to `delivered_kB_s`.
- [ ] **1.8 Implement the URI cache + resume** per the Solution section: cache the last valid URI
      on stream attach, `TagValid` → `Play(cachedURI)`, no URI → send nothing, cache never
      cleared by `playback: stopped`, and the `Play("")` path removed.
- [ ] **1.9 Tests.** Unit tests for the cache: set on stream, updated on hot-swap, *not* cleared by
      `playback: stopped`, `TagValid` with an empty cache is a no-op, and `Play("")` is never sent
      on any path. Keep the mock able to re-place a tag **without** re-requesting the stream — that
      is the exact real-world case that failed, and it must be reproducible in tests.
- [ ] **1.10 Deploy and verify.** Bump `vars/tc-radio-shim.yml` to the new tag, commit only that
      file, run the play, then repeat the 5-cycle repro. Acceptance: audio resumes **within
      ~1 s** of every re-placement with no dependence on a track boundary; no
      `play requires a valid playable Spotify URI` anywhere in the log; a *different* figurine
      still switches content correctly; and the new non-silence metric confirms audible output
      during the previously silent window.

# Issue 2 — The pipe is content-blind: silence floods it, and drops can eat music

Separate from Issue 1, but the same root cause as the "song skips ahead" report: **the shim
never looks at the samples it is shipping.** Everything below follows from that one fact.

## Correction to an earlier assumption

The silence is **not** being dropped. It is being forwarded at full rate.

- `virtual_out` is a PulseAudio **null-sink** (`internal/audio/pulse.go:420`) and the shim records
  its monitor (`internal/audio/monitor.go:55-62`). A monitor on a sink that nothing writes to
  does not go idle — PulseAudio keeps producing samples in real time, forever. So while Soloist is
  paused the shim is reading 88200 B/s of zeros.
- Producer rate equals consumer rate, so the buffer neither fills nor drains, no chunks are
  dropped, and the pipeline reports `delivered_kB_s=84.4 drop_ratio=0.00`.
- That is precisely the healthy-looking log we saw while the box was mute. The metrics were not
  wrong, they were measuring the wrong thing: bytes forwarded, not audio audible.
- What *has* actually been dropped is everything produced while **no consumer is attached**
  (`drop_ratio=0.48` at startup, before the first `/stream`).

The operator's underlying worry is nonetheless correct: the drop policy at
`internal/audio/recorder.go:178-183` is content-blind. It discards the newest chunk whether that
chunk is silence or the middle of a song. Today it only fires with no consumer attached, but
nothing prevents it eating real music the moment a consumer stalls.

## Mechanism (four distinct causes, all real)

- **A — sustained silence is streamed in full.** Described above. Cost: wasted bandwidth, a
  permanently "busy" pipeline that hides real failures, and 12 s of stale zeros waiting in the
  buffer for the next connect.
- **B — content-blind drop on a full buffer.** Non-blocking send in `pump`
  (`internal/audio/recorder.go:178-183`): full channel → discard newest, count it. The consumer
  keeps draining the oldest, so delivered audio falls behind live and lurches when the lag is
  worked off. The channel holds `defaultBufferLen = 256` chunks = 1 MiB ≈ **11.9 s** at 22050 Hz
  stereo s16 (`internal/audio/recorder.go:14-17`).
- **C — deliberate flush on every `/stream` connect.** `handleStream` calls
  `flushStalePreFill(src)` (`internal/server/stream.go:70`), draining the buffer via
  `PulseRecorder.Flush()` (`internal/audio/recorder.go:127-142`). An instant forward jump of up to
  ~12 s on **every** (re)connect — i.e. exactly on re-placement and hot-swap. This is the "however
  big the buffer" jump that was reported. Note it is mostly throwing away *zeros* per A.
- **D — size-only flush costs up to ~1.5 s of latency.** `streamChunks` wraps the writer in
  `bufio.NewWriterSize(w, 131072)` and only flushes when that buffer fills or the stream ends
  (`internal/server/stream.go:110-128`). 131072 B / 88200 B/s ≈ **1.49 s** of audio can sit in the
  writer before reaching the wire. The 128 KiB threshold is deliberate — it amortises teddycloud's
  per-segment read cost — but it should not be the *only* flush trigger.

## Solution

Two ideas, and they reinforce each other: **stop shipping audio the shim knows is silence, and
never drop audio it knows is signal.** The shim can classify a 4096-byte s16le chunk with a
peak/RMS check for a few operations — negligible next to the PulseAudio work already being done
per chunk.

- **Gate on known intent first, RMS as backstop.** The shim already learns from SSE whether a
  figurine is on the box (`playback: stopped` / `TagValid`). That is *exact* state, so "should
  there be sound right now" should be driven by it, not inferred. Use RMS only for the cases
  intent cannot see (e.g. a track that simply ends). Never the other way round.
- **Trickle, don't flood.** Once the source has been silent for a few hundred ms, stop enqueuing
  and emit at most one chunk per ~1 s. This keeps ffmpeg, caddy and the HTTP connection fed —
  a totally starved pipe risks being treated as a broken stream, and caddy sits in the path — while
  making it impossible for 12 s of stale zeros to accumulate. A 46 ms chunk of zeros inserted
  *while nothing is playing* is inaudible; the trickle only ever runs in the already-silent state,
  so it never punches a hole in music.
- **Content-aware drop as the safety net.** On a full buffer: a silent incoming chunk is dropped
  freely (lossless by definition); a *signal* chunk is not discarded — block briefly (bounded,
  e.g. 250 ms) for the consumer to catch up, and only then drop, counted separately and loudly.
- **Counters split by cause.** `dropped_silence`, `dropped_signal` and `flushed` are three
  different failure modes and must never be summed into one `dropped_s`.
- **Flush on a timer as well as on size.** Keep the 128 KiB batching for throughput, and add a
  periodic flush (e.g. every 100–200 ms) so latency no longer scales with the batch size — which
  also stops trickled silence from sitting unflushed in the `bufio`.

Net effect: C largely dissolves (there is nothing stale to flush), B stops being able to eat
music, and A stops consuming bandwidth. Latency improves as a side effect of D.

## Design tension that remains

- **Live edge** (current behaviour): start at the newest audio, accept a jump. Low latency —
  mandatory for a Toniebox, where 12 s of delay would feel broken.
- **Drain the buffer** (play from ~12 s ago): continuous audio, no jump, but adds up to ~12 s of
  latency.

Recommendation: keep the live edge. With the trickle in place the buffer holds only real audio, so
`Flush()` throws away seconds of *music* at worst rather than 12 s of zeros, and the buffer can
then be shrunk to bound the worst case. Buffer size stays a genuine tuning decision, not a free
parameter.

## Tasks

- [ ] **2.1 Correlate the jump with a cause.** Have the operator note the exact moment a jump is
      heard. Coincides with a `/stream` attach (see 1.1's log lines) → cause **C**. Happens
      mid-playback with `dropped_s > 0` → cause **B**. Record at least three instances.
- [ ] **2.2 Split the counters by cause** (`dropped_silence` / `dropped_signal` / `flushed`) and
      report seconds-of-audio for each. Without this, A/B/C are indistinguishable after the fact.
- [ ] **2.3 Add the RMS/peak measurement to the pump** (needed by 1.7 too — implement once, in
      `internal/audio/recorder.go`, and expose it to both). Report `signal_kB_s` next to
      `delivered_kB_s` so "bytes forwarded" and "audio audible" stop being the same statement.
- [ ] **2.4 Implement intent gating + silence trickle** per the Solution section, driven by the SSE
      playback state with RMS as backstop.
- [ ] **2.5 Verify teddycloud tolerates a starved pipe** before relying on the trickle: confirm
      that a multi-second gap in the `/stream` body does not make ffmpeg error out or time the
      connection out through caddy. This is the main risk in the whole design and must be tested
      on the box, not assumed.
- [ ] **2.6 Implement the content-aware drop policy** with the bounded block, and verify that a
      deliberately stalled consumer produces `dropped_signal > 0` rather than silent music loss.
- [ ] **2.7 Add the periodic flush** and measure the latency improvement against the current
      ~1.5 s worst case.
- [ ] **2.8 Measure real buffer occupancy** (high-water mark) during normal playback to size the
      buffer properly, then re-check that steady state still shows no `dropped_signal`.
- [ ] **2.9 Verify on hardware** with a long uninterrupted play plus several lift/place cycles,
      confirming no audible jump, no drop growth, and audible output immediately on re-placement.
- [ ] **2.10 Keep this out of the Issue 1 commit.** One commit per issue, as agreed.

# Issue 3 — Skipping does nothing

## Symptom

Forward/backward skip gestures do not skip tracks. Zero skips in the log.

## Evidence already collected

- `sselistener: skip_next` / `skip_prev`: **0 occurrences** in the entire container log.
- `ignoring knock event`: **0 occurrences** → no `knock` event with an unrecognised value either.
- Therefore **no `knock` event has ever reached the shim on this box.**
- The event name and payload are nevertheless confirmed correct: the byte-for-byte real capture
  `docs/research/teddycloud-sse-capture.txt` contains
  `event: knock` / `{"type":"knock","data":"forward"}` (line 1516) and `"backward"` (line 1646).
- That capture holds 293 `rtnl-raw-log2` + 20 `rtnl-raw-log3` events but only **2** knocks. Each
  knock is preceded by a burst of raw accelerometer events and one `rtnl-raw-log3`. Toniebox
  classifies a *raw accelerometer pattern* into `knock` only when a threshold is crossed — the
  ratio (2 knocks / 313 raw events) shows the gesture is finicky and easy to miss.
- Independent constraint from soloist's own `playback_state` event:
  `"available_actions":{"add_to_queue":{},"play":{},"set_repeat":{},"shuffle":{},"skip_next":{}}`
  — **`skip_prev` is not offered** in that context. Backward skip may be unsupported regardless
  of the gesture.

## Ranked hypotheses

- **H1 (most likely).** The physical action being performed never crosses teddycloud's knock
  threshold, so no `knock` event is produced. VLS1's live log shows a steady stream of
  `rtnl-raw-log2` noise, so the accelerometer channel is alive — only the classification fails.
- **H2.** `knock` fires but the value is neither `forward` nor `backward` (e.g. a device-specific
  token), and the shim drops it. This *would* log `ignoring knock event`, which has never
  appeared — so this is currently unlikely, but the capture in 3.1 settles it.
- **H3.** The gesture works and the command is sent, but soloist rejects it: `skip_prev` absent
  from `available_actions`, or the queue/context does not permit a skip. This is the likely
  explanation for *backward* skip even after the gesture is fixed.
- **H4.** Direction is inverted: the Tonies app's "Fast-forwarding & rewinding" setting swaps
  which side goes forward, so forward and backward are swapped relative to the shim's mapping.

## Tasks

- [ ] **3.1 Gesture capture session.** With the shim log streaming and a second SSE consumer
      attached, have the operator perform, in order, ~10 s apart: tilt forward, tilt backward,
      firm tap right side, firm tap left side, double tap right, double tap left.
      For each: record whether a `knock` event appears in the SSE capture, its value, the
      preceding `rtnl-raw-log*` burst length, and what the shim logged.
      This produces the gesture → value table for *this* box. Acceptance: at least one physical
      action reliably yields `knock forward` and one yields `knock backward`.
- [ ] **3.2 Pin down the direction convention** from 3.1 and reconcile with the Tonies app
      "Fast-forwarding & rewinding" setting. Document the box-specific mapping.
- [ ] **3.3 Verify soloist acceptance.** For each working gesture, read the shim log for
      `soloist ws event … "command_result"` or an `error` for `skip_next` / `skip_prev`, and
      check the current `available_actions`. Record explicitly whether `skip_prev` is ever
      offered. Acceptance: known yes/no per direction, from evidence rather than assumption.
- [ ] **3.4 Decide the mapping for unsupported directions.** If `skip_prev` is genuinely
      unavailable (H3), do not silently drop the gesture. Options, in preference order:
      (a) use the closest supported action if one exists; (b) log the rejection clearly and
      document the asymmetry; (c) leave it unimplemented but say so in the README. This is a
      product decision — **needs the operator's input.**
- [ ] **3.5 Fix the mapping** if 3.1/3.2 show different event names or values than
      `knock forward` / `knock backward`; update the listener, its tests, the mock, and
      `docs/research/teddycloud-sse-events.md`.
- [ ] **3.6 Fix the stale documentation.** `docs/research/teddycloud-sse-events.md:115` still says
      knock events "are irrelevant to the control path", contradicting the corrected table at
      lines 123–124. Remove the contradiction while editing that file in 3.5.
- [ ] **3.7 Tests and deploy.** Unit tests for the corrected mapping; extend the mock to emit
      the verified values; bump the image, deploy, and repeat the 3.1 gesture matrix against the
      deployed build. Acceptance: a forward gesture advances the track within ~1 s, and the log
      shows `sselistener: skip_next` followed by a `command_result`, not an error.

# Issue 4 — `position_sync` observability gap (low priority)

`soloist ws event … position_sync` appears twice at `07:06:13` (`position_ms=2`) and then never
again, even though teddycloud's ffmpeg proves audio keeps advancing (`speed=1.03x`). Playback is
fine; the position feed is not observable. Task 1.5 does not depend on it: it reads
`playback_state`, which still arrives on every play.

- [ ] **4.1** Determine whether the soloist WS subscription stops delivering position updates
      or the shim stops logging them.
- [ ] **4.2** If it is only a logging gap, emit a periodic (e.g. 30 s) one-line playback status
      — track title, position, state — so future sessions can be diagnosed from the log alone.
- [ ] **4.3** While there: `/healthz` currently reports `ok` even when no consumer is attached
      and audio is silent. Make it reflect consumer state so this class of failure is visible
      without log archaeology. Note the underlying limitation: `/healthz` cannot observe soloist
      silence, only shim-side state.

---

## Repro harness

Terminal 1 — shim log, filtered to the control path and the pipeline:

```sh
ssh opc@130.61.85.57 \
  'sudo -n podman logs -f --since 2m tc-radio-shim 2>&1 | grep -E "sselistener|pipeline:|soloist ws event|soloist play failed|command_result|available_actions"'
```

Terminal 2 — raw SSE from teddycloud, to see every event including the ones the shim ignores:

```sh
ssh opc@130.61.85.57 'sudo -n podman exec teddycloud curl -sN --max-time 600 http://localhost:80/api/sse'
```

Terminal 3 (optional) — teddycloud's own view of stream requests and ffmpeg restarts:

```sh
ssh opc@130.61.85.57 'sudo -n podman logs -f --since 2m teddycloud 2>&1 | tail -40'
```

Caveat: a **second** concurrent SSE consumer is assumed to work (it did against the OCP
deployment); if teddycloud only serves one subscriber, fall back to the shim's own DEBUG log,
which prints the name of every unhandled event — enough to see that `knock` did or did not
arrive.

Tasks 1.3, 2.1 and 3.1 all need the operator physically at the box. Run them as **one combined
session** to avoid multiple trips.

## Working rules for this work

- One commit per issue, Conventional Commits, matching the existing history. Issues 1 and 2 must
  not share a commit.
- Do **not** touch the other agent's in-flight work: `.mise.toml`, `Containerfile`,
  `.dockerignore`, `docs/STRUCTURE.md`, and the deleted `container/ocp/*` files.
- The homelab repo has an unrelated modified file
  (`app-deployment/roles/teddycloud-config/files/config/tonies.custom.json`); never commit it
  alongside an `app_image` bump.
- Always confirm the deployed image is the one you just built (`podman inspect
  tc-radio-shim --format '{{.ImageName}}'`) before interpreting any log.
