# Ideal shim behaviour with Soloist as backend — and how the current implementation differs

**Date:** 2026-09-28
**Scope:** the control path (`GET /api/sse` → shim → Soloist WebSocket), the data path
(`GET /stream` → Soloist PCM), and the observability that joins them.
**Inputs:** `docs/teddycloud-static-analysis-sse.md` (teddyCloud v0.7.0, `78c3b38`),
`docs/research/*` (live captures), `plan_debug_sound.md`, and the shim source at `af14543`.

**Claim tags used throughout:**

| Tag | Meaning |
|---|---|
| **[P]** | *proven* — determined by teddyCloud's or the shim's source |
| **[E]** | *empirical* — observed on a real box; reproducible, but not derivable from code |
| **[H]** | *hypothesis* — asserted here, still needs a measurement before it becomes design |

---

## Table of contents

1. [Executive summary](#1-executive-summary)
2. [The two planes, and the asymmetry that defines the problem](#2-the-two-planes-and-the-asymmetry-that-defines-the-problem)
3. [Constraint set: what teddyCloud and Soloist actually tell us](#3-constraint-set-what-teddycloud-and-soloist-actually-tell-us)
4. [Part A — How the shim should behave](#4-part-a--how-the-shim-should-behave)
5. [Part B — Where the current implementation differs](#5-part-b--where-the-current-implementation-differs)
6. [Part C — Audit of `plan_debug_sound.md` Issue 1](#6-part-c--audit-of-plan_debug_soundmd-issue-1)
7. [Part D — What the shim cannot fix, and what only the operator can decide](#7-part-d--what-the-shim-cannot-fix-and-what-only-the-operator-can-decide)
8. [Part E — Proposed work sequence](#8-part-e--proposed-work-sequence)
9. [Appendix A — Contradictions to retire](#9-appendix-a--contradictions-to-retire)
10. [Appendix B — Fact index](#10-appendix-b--fact-index)

---

## 1. Executive summary

The shim's basic architecture is right and should be kept: teddyCloud pulls the audio from the shim
over HTTP, and the shim is the only thing that talks to Soloist. `content.json` →
`source: http://shim:8080/stream?spotify_uri=…` → teddyCloud's ffmpeg → TAF → box is the correct
inversion of control **[P]** (`docs/teddycloud-static-analysis-sse.md:1291`). Bypassing teddyCloud's
MQTT control plane is also defensible here (see [§7.3](#73-why-not-mqtt)).

What is wrong is the **control path's mental model**. Three beliefs in the current code and in
`plan_debug_sound.md` do not survive contact with the source:

1. **"The SSE feed tells the shim what to play."** It cannot. No SSE event carries a URI, ever — not
   `TagValid`, not `playback`, not `Content*` **[P]**
   (`docs/teddycloud-static-analysis-sse.md:1299-1310`, `:1440-1450`). The only URI on the wire is the
   `spotify_uri` query parameter of `GET /stream`. Every attempt to derive content from SSE is
   guesswork.

2. **"Silence means the control path failed."** teddyCloud emits **zero** SSE events about the stream
   it is serving — not on start, not on teardown, not on error **[P]**
   (`docs/teddycloud-static-analysis-sse.md:1299-1309`: `rg -n "sse_" src/handler_cloud.c` →
   no matches). And the `/stream` consumer lifecycle, which the shim *does* own, is a strictly
   better liveness signal than anything on the feed.

3. **"`TagValid` means 'a figurine was placed, therefore play'."** `TagValid` is the box's own
   verdict that *some* tag is known **[P]**
   (`docs/teddycloud-static-analysis-sse.md:578-580`); it fires for a figurine that has nothing to do
   with the current stream, and its UID is mangled for a whole class of real tags **[P]** (§11.1,
   `:2392-2428`).

The result is that today's shim runs a **four-event, stateless, edge-triggered** control path
(`internal/sselistener/listener.go:229-265`) whose only content-bearing action is a guaranteed
protocol error (`{"type":"command","command":"play","uri":""}` — Soloist answers
`play requires a valid playable Spotify URI`), and an observability surface that is *structurally
incapable* of showing whether a `/stream` request ever arrived.

### The five changes that matter

| # | Change | Rationale |
|---|---|---|
| **1** | `/stream` becomes the **only** place content is decided; the URI is latched there; SSE never sets content | It is the only place a URI exists **[P]** |
| **2** | `play` gets a real **resume** form (omit `uri`) instead of `"uri":""` | Soloist documents `uri` as *optional* — *"Resume or play a Spotify URI"* (`docs/research/spotify-soloist.md:163`). The shim's non-`omitempty` field (`internal/soloist/connector.go:40-44`) converts the documented resume into a hard error **[S]** |
| **3** | **Consumer lifecycle** (`/stream` attach/detach) becomes the primary liveness signal, with SSE `playback:*` as a secondary hint | teddyCloud's stream teardown is *driven by* `playback:stopped` **[P]** (§8.6), so the consumer detach is the *same fact observed at the layer the shim owns* |
| **4** | Soloist's WS events are **parsed into a state machine** instead of being debug-logged | `playback_state` / `track_changed` / `position_sync` / `error` are the only authoritative view of the producer **[P]** (`docs/research/spotify-soloist.md:130-147`); today they are discarded (`internal/soloist/supervisor.go:303`) |
| **5** | `/stream` lifecycle + Soloist state + non-silence all become **logged and reported** | The current telemetry is byte-shaped, so digital silence is indistinguishable from music — which is exactly how the mute bug hid **[S]** (`plan_debug_sound.md:112-120`) |

---

## 2. The two planes, and the asymmetry that defines the problem

```
                        ┌─────────────────── control plane (one-way observation) ───────┐
 Toniebox ──RTNL──▶ teddyCloud ──GET /api/sse──▶ sselistener ──▶ Soloist WS (commands)
                        │                                                       │
                        │  ✗ no URI       ✗ no "stream started"                    │
                        │  ✗ no tag removal event                             ◀──┘
                        │  ✗ no "tag on box" readback                     Soloist events
                        ▼                                                       │ (all discarded today)
 ┌──────────────────── data plane (teddyCloud pulls) ─────────────────────┐   │
 │  GET /v1/freshness-check                                              │   │
 │  GET /v2/content/<ruid>  ──▶ ffmpeg -i "http://shim:8080/stream?…" ──▶ shim ──▶ PCM
 │  ◀── 200, Content-Length: 251658239 (~6 h), non-chunked, held open ───┘   │
 │  ◀── TAF (60 ms VBR Opus @ 48 kHz), range-requested by the box               │
 └──────────────────────────────────────────────────────────────────────────┘
```

Two facts make this shape unusual, and both are **[P]**:

- **`src/handler_cloud.c` contains no `sse_sendEvent` call.** The freshness check, the content
  request, the ffmpeg spawn, the teardown — all invisible on SSE
  (`docs/teddycloud-static-analysis-sse.md:1299-1309`). The data plane is a black box to the control
  plane by construction.
- **There is no server→box command channel at all.** teddyCloud's own `MQTT_CONTROL.md:9-20` says so.
  Volume *limits*, LED mode, slap-to-skip and `CmdStop` are the entire reachable surface
  (`:1914-2040`).

⇒ The shim sits in a genuinely awkward position: it can **observe intent** and it can **command the
producer**, but it cannot know whether the box is currently consuming. The only way to close that gap
is to use the one channel it *does* own end to end: the `/stream` HTTP connection.

---

## 3. Constraint set: what teddyCloud and Soloist actually tell us

Everything the design may rely on. `…:NNNN` references are line numbers in
`docs/teddycloud-static-analysis-sse.md`.

### 3.1 Identity — what is playing

| Fact | Tag | Source |
|---|---|---|
| `TagValid` payload is a 16-char hex uid; **no URI is ever on SSE** | **[P]** | `:540-576` |
| `TagValid` is emitted for **any** tag the box considers known — no relation to the current stream | **[P]** | `:578-580` |
| For any tag whose first byte is ≥ `0x80`, `read_big_endian64` sign-extends → payload is `FFFFFFFF…` and **distinct tags collapse onto one uid** | **[P]** | `:2392-2428` (§11.1) |
| The only URI in the system is `GET /stream?spotify_uri=…` | **[P]** | `:1440`, and `internal/server/stream.go:18` |
| A radio tonie produces the **same** SSE events as a cached TAF; there is no source-type branch in the SSE path | **[P]** | `:1311-1317` |
| `ContentAudioId` for a stream = `time(NULL) - 0x50000000`, always `< 0x50000000` ⇒ a reliable **radio fingerprint** carrying the ffmpeg start time | **[P]** | `:1327-1360` |
| Whether the box emits the audio-id report for a live stream is unknown | **[E]** | `:1404-1433` (§8.4) |
| `ContentTitle` is `"Unknown"` for a radio tonie without a resolvable `tonie_model`; `ContentPicture` is then **absent** — not a fallback | **[P]** | `:1362-1388` |
| `playback:starting` immediately falls through to `playback:started` (missing `break`) ⇒ `starting` carries zero information; never count occurrences | **[P]** | `:620-627` |
| No SSE event carries a box identifier; with N boxes every subscriber sees the interleaved union | **[P]** | `:767-777` |

### 3.2 Liveness — is anything supposed to be audible

| Fact | Tag | Source |
|---|---|---|
| Guaranteed for every stream: `rtnl-raw-log2`/`-log3`, `TagValid`, `playback:starting`+`started`, `playback:stopped`, `keep-alive` | **[P]** | `:1772-1781` |
| **Tag removal emits no SSE event.** `tbs_tag_removed()` has no `sse_sendEvent` at all | **[P]** | `:728-737`, `:1456-1478` |
| The only lossless tag-removal signal on SSE is the raw frame `rtnl-raw-log2` with `function_group == 15 && (function == 8630 \|\| function == 16067)` | **[P]** | `:1826`, `:2333-2335` |
| `TagInvalid` **does exist** (`toniebox_state.c:36`, from RTNL 15/8646 CC3200 and 15/15452 ESP32) — it has simply never been observed on this box | **[P]** / **[E]** | `:573-574`, `:482-486` |
| `playback:stopped` has **two** sources: log3 `field2=13`, **and** log2 `22-6480` with a system-sound path (charger plug, low battery, install hint, playback finished) | **[P]** | `:628-635` |
| `playback:stopped` is therefore **not** a reliable "figurine lifted" or "end of queue" marker | **[P]** | `:634-635` |
| On `playback:stopped` teddyCloud sets `stream_ctx.active = false` (`stop_on_playback_stop` is set `true` for both stream paths and never reset) ⇒ **the ffmpeg stream dies and the HTTP response to the box is torn down** | **[P]** | `:1498-1530`, `:1480-1496` |
| The transport backstop fires independently: the socket is non-blocking, so a box that stops reading fills the buffers, `send()` returns `EAGAIN`, and the stream loop breaks | **[P]** | `:1532-1592` |
| ⇒ **teddyCloud cannot keep a `/stream` HTTP connection open across a figurine lift.** H1 in `plan_debug_sound.md` is refuted | **[P]** | `:1590-1592`, `:1626` |
| Residual: the box may keep playing its **own** buffered audio; teddyCloud cannot reach it | **[P]** | `:1616-1621` |
| There is **no REST readback** for `tag.valid`, `tag.uid`, `tag.audio_id`, or "is a stream active" | **[P]** | `:2244-2247` (§9.8) |
| `internal.last_ruid` is "last ruid seen", rewritten on removal ⇒ useless as "currently placed" | **[P]** | `:1829`, `:2534` |
| No `id:`, no `retry:`, no replay, no `Last-Event-ID` ⇒ **every reconnect loses the gap window**; the client must be level-triggered and re-read state | **[P]** | `:358-367`, `:2270-2273` |
| Payloads are **not JSON-escaped** ⇒ a title containing `"` or `\` breaks `JSON.parse`, and a newline desyncs the frame; a parse failure must be treated as a dropped event | **[P]** | `:315-342`, `:2493-2510` |
| Max 8 subscribers process-wide; the 9th gets `503`, which is **fatal** to a browser `EventSource` | **[P]** | `:164-195`, `:2262-2265` |
| `keep-alive` is the only liveness signal, every ~16 s; nothing for ≳30 s ⇒ the connection is dead | **[P]** | `:443-452`, `:2266-2269` |

### 3.3 The stream itself

| Fact | Tag | Source |
|---|---|---|
| `source` containing `://` ⇒ `CT_SOURCE_STREAM`; the URL is handed **raw to ffmpeg** | **[P]** | `:1269-1293` |
| Only **one** `stream_ctx` per overlay ⇒ at most one radio per box; a second content request zeroes the first's `active` flag | **[P]** | `:1488-1491`, `:2223-2221` (§9.7) |
| The response is `200` with a **fabricated** `Content-Length: 251658239` (~6 h) and the TAF header carries `stream_max_size - 4096`; on teardown the header is rewritten with the *true* size ⇒ the box sees a **truncated download** | **[P]** | `:1709-1716`, `:1594-1614` |
| `Range.start == 0` truncates the `.stream` file; `Range.start != 0` **appends** and reads the audio id back from the existing header | **[P]** | `:1651-1683` |
| `encode.ffmpeg_stream_restart = false` (default) ⇒ the box is told the same total for 6 h and **resumes where it left off**, audibly replaying radio it already downloaded | **[P]** | `:1685-1708` |
| Before the first byte: `ffmpeg_sweep_delay_ms` (2000, if sweep enabled) + `ffmpeg_stream_buffer_ms` (2000) ⇒ **≥ 2 s of teddyCloud-side latency on every ffmpeg start** | **[P]** | `:2201-2212`, `:1538-1547` |
| `.stream` files are never deleted by teddyCloud, even on ffmpeg failure | **[P]** | `:1630-1650` |
| Only `MQTT CmdStop` can stop a stream deliberately; REST cannot | **[P]** | `:1839-1842`, `:2042-2092` |

### 3.4 Soloist

| Fact | Tag | Source |
|---|---|---|
| `play` takes an **optional** `uri`: *"Resume or play a Spotify URI"* | **[E]** | `docs/research/spotify-soloist.md:163` |
| The shim sends `"uri":""` (no `omitempty`) ⇒ Soloist answers `play requires a valid playable Spotify URI` **[E]** (observed 4× in the VLS1 log) | **[E]/[S]** | `internal/soloist/connector.go:40-44`; `plan_debug_sound.md:49,52` |
| `command_result` means *"the command was accepted"*, **not** that state changed | **[E]** | `docs/research/spotify-soloist.md:147` |
| Server → client events: `auth_state`, `playback_state`, `track_changed`, `playback_changed`, `volume_changed`, `device_changed`, `context_changed`, `options_changed`, `position_sync`, `queue_changed`, `command_result`, `error` | **[E]** | `docs/research/spotify-soloist.md:130-145` |
| Query commands: `get_state`, `get_queue`, `get_auth_state` — these **return the event directly and produce no `command_result`** | **[E]** | `docs/research/spotify-soloist.md:151-157` |
| `playback_state.available_actions` on a radio context contains `skip_next` but **not** `skip_prev` | **[E]** | `plan_debug_sound.md:322-325` |
| Soloist restores its **persisted** volume (40/100) at startup unless overridden | **[E]** | `docs/research/spotify-soloist.md:205` |
| Soloist's WS has no auth, no TLS, no Origin check | **[E]** | `docs/research/spotify-soloist.md:122` |

---

## 4. Part A — How the shim should behave

### 4.1 Identity: `/stream` decides content, and only `/stream`

**Rule.** The currently-loaded URI is set by exactly one event: the `spotify_uri` query parameter of
an incoming `GET /stream`. It is latched (uri + attach timestamp + consumer count) and re-latched on
every re-attach. Nothing else in the system may write it.

**Why.** Identity is provably absent from the SSE feed (`:1299-1310`, `:1440-1450`). Any
content-inference path is a guess that happens to be right most of the time on a single-box,
single-figurine deployment, and wrong in exactly the cases that matter — a second figurine, a
non-radio figurine, a box that classifies a tag the shim cannot identify (`:2392-2428`).

**Consequences.**

- `TagValid` becomes a **box-interaction edge**, useful for telemetry, deduplication, and (later) for
  correlating a UID to a RUID via `GET /api/getTagInfo` — never for choosing content.
- The UID carried by `TagValid` is latched, because it is the only handle on *which* figurine is
  involved. Today it is discarded before it is even parsed (`internal/sselistener/listener.go:234`).
- `ContentAudioId < 0x50000000` is used as a **radio fingerprint** and, via
  `ContentAudioId + 0x50000000`, as the teddyCloud-side ffmpeg start time. That gives the shim, for
  free: stream age, and detection of a teddyCloud-side restart (a *rising* audio id on an unchanged
  URI). This also answers §8.4 empirically, from production logs.
- `context_changed` from Soloist is the **producer-side** view of the same fact. When both are
  available they should agree; a disagreement is a bug report waiting to happen, so both are logged.

### 4.2 Liveness: three independent facts, each read from the right layer

The shim needs to answer "should there be sound right now?" — but that question has **three**
different answers depending on who is asked, and conflating them is the root of Issue 1 and Issue 2.

| Question | Authoritative source | Why that layer |
|---|---|---|
| *Is teddyCloud pulling audio from me?* | **The `/stream` consumer itself** (`internal/server/stream.go:47-54`) | It is the transport. teddyCloud emits nothing on SSE for it (`:1299-1309`), yet it *does* tear the connection down on `playback:stopped` (`:1498-1530`). Consumer-detach is the same fact, observed at the layer the shim owns. |
| *Is the box currently playing something?* | **SSE `playback:started` / `playback:stopped`** latch | These are unconditional and source-type-agnostic (`:1440-1441`, `:1722-1781`). `stopped` is ambiguous (`:628-635`) but "possibly stopped" is the right reading for a *hint*. |
| *Is Soloist actually producing sound?* | **Soloist `playback_changed`/`playback_state` + non-silence in the PCM** | Only the producer knows whether it is playing, buffering, or stalled; and only the samples know whether the output is audible. |

**Rule.** These three are kept as three separate latches and never collapsed into one boolean. Each
drives a different decision:

- consumer-attached → whether to spend bandwidth on `/stream` at all;
- box-playing → whether a `playback:stopped` should have stopped *Soloist* (it should not — see
  [§4.3](#43-playbackstopped-must-not-pause-soloist));
- soloist-playing + RMS → whether the telemetry line says "healthy" or "sending zeros".

**Corollary — the watchdog problem is solved by the right clock.** `plan_debug_sound.md:112-120`
complains that `delivered_kB_s=84.4` cannot distinguish music from silence. The fix is not a
sophisticated silence detector; it is to report Soloist's own state next to the byte counter, so
"bytes are flowing" and "a producer is playing" stop being the same statement. RMS is still worth
having, because Soloist says it is playing while Spotify has stalled — but it is the *second* line of
defence, not the first.

### 4.3 `playback:stopped` must not pause Soloist

Today's mapping is `playback:stopped` → `Soloist pause()`
(`internal/sselistener/listener.go:235-238`). Three independent reasons to stop doing that:

1. **It is not a lift signal.** It also fires for charger-plug, low-battery and playback-finished
   system sounds (`:628-635`). Mapping it to `pause` means the radio stops when someone plugs the
   box in.
2. **Pausing destroys the only state that makes recovery cheap.** Soloist owns the playback context;
   `pause` is a state transition, and the only way back is a `play`, and the only URI we have is the
   latched one. Every lift therefore becomes a pause→resume round trip through a component that
   cannot be re-primed from the SSE side.
3. **The box's silence is already handled one layer down.** teddyCloud kills its own ffmpeg on
   `playback:stopped` (`:1498-1530`) and the HTTP response is torn down (`:1532-1592`). The shim sees
   the consumer detach. Nothing needs to be paused for the box to go quiet — and the plan itself
   notes this possibility at `plan_debug_sound.md:105-109` without following it through.

**Rule.** `playback:stopped` → set `boxPlaying = false`, log it, and **let the transport do the
rest**. Do not send a Soloist command. The producer keeps its context so that the re-placement is a
`/stream` re-attach with the same URI (a `play` that Soloist treats as a no-op-or-resume) rather than
a cold reconstruction.

*Open question this creates, and it is the one that must be measured:* does `play <same-uri>` while
already playing that URI restart from 0:00? `plan_debug_sound.md:94-101` asks this correctly
(`playback_state.position.position_ms` after a mid-track re-placement). Until it is answered, the
shim should not send `play` on re-attach at all if the consumer URI is unchanged and Soloist already
reports that URI as loaded — i.e. **idempotence decided from producer state, not from the shim's own
memory.**

### 4.4 A real "resume", instead of a guaranteed error

`play` has an optional `uri` (`docs/research/spotify-soloist.md:163`). The shim's `playCommand`
has no `omitempty` (`internal/soloist/connector.go:40-44`), so `Play("")` puts `"uri":""` on the
wire and Soloist rejects it with `play requires a valid playable Spotify URI` — a guaranteed error
on every single placement **[E]**, observed four times in the VLS1 log.

**Rule.** Model resume as an *absent field*, not an empty string:

```
Play(uri)  → {"type":"command","command":"play","uri":"spotify:track:…"}   when uri != ""
Resume()   → {"type":"command","command":"play"}                            when no uri is wanted
```

That gives the shim a legitimate, protocol-supported answer to "figurine back on the box, resume
whatever is loaded" — and it moves the decision to the component that actually holds the context.

**This is a testable claim, not a proven one.** It requires confirming on the box that Soloist
(a) resumes rather than errors when `uri` is absent, and (b) says something unambiguous when there
is nothing to resume. See [§8](#8-part-e--proposed-work-sequence), step 0.

**Until it is confirmed:** delete the `Play("")` call rather than replace it with the plan's
URI-cache variant ([§6.3](#63-the-plan-proposed-cache-and-resume)). A placement with no latched URI
should send nothing.

### 4.5 Command legality, and honest failure

| Box gesture / event | Correct action | Notes |
|---|---|---|
| `knock:forward` | `skip_next` | `knock` is box-classified; direction is decided by the box, not by teddyCloud (`:781-802`) |
| `knock:backward` | `skip_prev` **only if `available_actions` offers it** | Otherwise log "unsupported in this context" and count it, rather than firing a command that will be rejected (`:322-325`) |
| `tilt:*` | ignore | A within-track seek with no Soloist equivalent on a radio context; the shim is right to ignore it — but three documents currently claim it is mapped |
| `pressed:*` | ignore | Confirmed on hardware (`plan_debug_sound.md:16`); correct. The *value* need not even be read |
| `TagValid` | latch UID, telemetry | **not** a play trigger |
| `TagInvalid` | clear `tagPresent` | exists **[P]**, never seen **[E]** — handle it, log it |
| `rtnl-raw-log2` grp 15 fn 8630/16067 | clear `tagPresent` | the only *lossless* removal signal on SSE (`:1826`) |
| `ContentAudioId < 0x50000000` | mark `streamStartedAt`, count teddyCloud restarts | (`:1327-1360`) |
| `playback:starting` | ignore | carries zero information (`:620-627`) |
| `playback:started` | `boxPlaying = true` | latch, never count |
| `playback:stopped` | `boxPlaying = false`, **no Soloist command** | (`:628-635`) |
| `VolumeLevel` / `VolumedB` | ignore for volume control | box-local speaker level; Soloist's gain is the only volume that matters (`:895-931`, `docs/research/spotify-soloist.md:205-209`) |
| anything else | rate-limited "unhandled" counter | not a per-event debug line |

**Rule.** A command that Soloist rejects must leave a *durable* trace: `command` name, correlation id,
Soloist's `command_result` or `error`. Today the shim logs intent *before* the write
(`internal/sselistener/listener.go:274`) and drops the outcome, so the log reads as
`sselistener: play` followed by an unrelated `error:` line from another goroutine with nothing linking
them.

### 4.6 Reconciliation instead of edge handling

The client contract is explicit: be level-triggered, be idempotent, re-read authoritative state after
every reconnect (`:2270-2273`). None of the three is implemented today
(`internal/sselistener/listener.go:109-133`).

**Rule.** The shim owns a small state machine and re-derives intent whenever any input changes:

```
loaded      : uri | none                ← written ONLY by /stream attach
consumer    : attached(since) | none    ← written by /stream attach/detach
box         : tagPresent(uid)? playing? ← SSE TagValid/TagInvalid/raw-log2/playback:*
soloist     : auth · status · track · position · available_actions ← Soloist WS events
```

The single derived predicate is `shouldBeAudible = consumer attached && soloist.status == playing`.
Everything else is telemetry. On every SSE reconnect and on every Soloist `(re)attach` the shim:

1. sends `get_state` and waits for `playback_state`;
2. compares Soloist's loaded context against the latched `loaded` URI;
3. sends **one** corrective command at most — and only if Soloist disagrees;
4. logs the reconciliation result, so "the shim and Soloist disagreed at 12:04:11" is a searchable
   string.

This is also the honest answer to "what if a `TagValid` was missed while the SSE connection was
down?" (`:2270-2273`). It cannot be recovered from the feed — but it does not need to be, because
content comes from `/stream` and liveness comes from the consumer.

### 4.7 Command delivery

- **Bounded pending queue while Soloist's WS is down.** A `knock` that arrives during a WS reconnect is
  otherwise lost forever (`internal/soloist/connector.go:33-35` returns `not connected`; the listener
  swallows it at `:277-279`). Soloist is re-dialled in 1–15 s (`:146-166`), so a queue of depth 1–4
  is sufficient; anything older than ~10 s should be dropped as meaningless.
- **Order matters: `activate` before anything else.** Today the connector is attached *before*
  `activate` is sent (`internal/soloist/supervisor.go:168-172`), so a command can be written ahead of
  `activate` on the same socket. Attach the writer only after `activate` is acknowledged.
- **Ping/pong or a read deadline.** No `SetReadDeadline` is ever set, so a half-open socket is only
  detected by the OS (`internal/soloist/supervisor.go`).
- **Jittered backoff** everywhere (`internal/audio/pulse.go:503-514`, `internal/soloist/pair.go:133-144`,
  `internal/sselistener/listener.go:131`) so a fleet does not reconnect in lockstep — and reset the
  attempt counter on a *successful* connect, which today never happens (`:131`).

### 4.8 The `/stream` handler

Order of operations, and what must be logged:

1. Parse and validate `spotify_uri` (400 + reason) — today `:20-26`, correct.
2. Acquire the single consumer slot; cancel and **log** the previous holder — today silent
   (`:47-54`).
3. Latch `loaded = uri`. **Log the URI, every time** — it is currently logged nowhere on success.
4. Decide idempotently whether `play` is needed, from Soloist's reported state
   ([§4.3](#43-playbackstopped-must-not-pause-soloist)).
5. Discard the stale pre-fill (today `:64-70`; correct, and the reason it exists is sound).
6. Serve at the **live edge**, with a periodic flush so latency does not scale with the batch size
   (today 128 KiB ≈ 743 ms of residue, `:100-111`).
7. On teardown: log detach, the byte count, the duration, and the terminal reason (client gone /
   superseded / source gone).

**The one thing that must not be true today:** `/stream` has exactly one log line and it is a warning
(`internal/server/stream.go:74`). Every hypothesis in `plan_debug_sound.md` is undecidable without
this instrumentation, and the plan says so itself (`:146-147`).

### 4.9 The audio plane

- **Live edge, not buffer drain.** 12 s of latency is unacceptable on a Toniebox; the plan reaches
  the same conclusion (`:266-276`) and is right.
- **But the buffer should only ever hold signal.** Once the producer is known-silent, stop enqueuing
  and emit a trickle; keep a short periodic flush so the trickle is not stuck in the `bufio`
  (`:2212` in the plan, and `docs/teddycloud-static-analysis-sse.md` gives no verdict — this is
  **the one item in Issue 2 that must be tested on the box before it is relied on**).
- **Content-aware drop.** The current policy discards the newest chunk unconditionally
  (`internal/audio/recorder.go:178-183`). A silent chunk may be dropped freely; a signal chunk should
  block briefly and only then be dropped, counted separately.
- **Counters split by cause** (`dropped_silence` / `dropped_signal` / `flushed`) so that after the
  fact one can tell a starved pipe from a full buffer from a reconnect flush.

### 4.10 Observability and health

| Signal | Purpose |
|---|---|
| `sse: connected` / `disconnected` / `events_in` per name / `reconnects` | The feed is currently invisible on connect |
| `stream: attach{uri,hot_swap}` / `detach{reason,bytes,dur}` | Makes H1/H2/H3 decididable |
| `soloist: {auth,status,track,position_ms,available_actions}` | Producer truth; replaces "is it silent?" guessing |
| `pipeline: {delivered_kB_s, signal_kB_s, non_silent_ratio, dropped_*, streams}` | "Bytes are flowing" ≠ "audio is audible" |
| `commands: {sent, acked, errored, pending}` per command | Closes the intent↔outcome gap |
| `/healthz` reflecting **all** subsystems with a reason registry | Today one last-writer-wins string (`internal/server/server.go:57-61`), so the Soloist path can clear a PulseAudio reason and vice versa |

A single periodic one-line status (track title, position, state, consumer, stream age) answers plan
Issues 1.7, 4.2 and 4.3 at once — and it should be at `INFO`, not `DEBUG`: the single most important
diagnostic in the system is currently invisible in production (`cmd/shim/main.go:205-212`).

---

## 5. Part B — Where the current implementation differs

Severity: **H** = causes or masks user-visible audio failure · **M** = wrong or unobservable under
real conditions · **L** = hygiene.

| # | Area | Ideal | Current | Where | Sev |
|---|---|---|---|---|---|
| G1 | Content identity | Only `/stream` sets the URI | `TagValid` also fires `Play("")`, racing `/stream`'s `play(uri)` | `internal/sselistener/listener.go:234` vs `internal/server/stream.go:72-76` | **H** |
| G2 | Resume | Absent `uri` field | `"uri":""` ⇒ guaranteed Soloist error | `internal/soloist/connector.go:40-44` | **H** |
| G3 | Liveness | consumer attach/detach is primary | no consumer state reaches the control path | `internal/server/stream.go:47-54` | **H** |
| G4 | `playback:stopped` | set a latch, no command | `Soloist pause()` — fires on charger/low-battery too | `internal/sselistener/listener.go:238` | **H** |
| G5 | Tag removal | `TagInvalid` + `rtnl-raw-log2` grp 15 fn 8630/16067 | both ignored, on a code comment asserting `TagInvalid` does not exist | `internal/sselistener/listener.go:19,263` | **M** |
| G6 | Soloist events | parsed into a state machine | debug-logged and discarded | `internal/soloist/supervisor.go:303` | **H** |
| G7 | Command delivery | correlate `command_result`/`error`; bounded queue while WS is down | fire-and-forget; errors swallowed; no queue, no retry | `internal/soloist/connector.go:29-38`, `internal/sselistener/listener.go:270-280` | **H** |
| G8 | `/stream` observability | attach/detach/uri/switch logged | one `Warn`, only on failure | `internal/server/stream.go:74` | **H** |
| G9 | SSE watchdog | read deadline, ~30 s | 2 min **and unreachable**: `Scan()` blocks the `select` | `internal/sselistener/listener.go:181-193` | **M** |
| G10 | SSE reconnect | jitter, reset on success, reconcile | no jitter, `attempt` monotonic forever, nothing re-asserted | `internal/sselistener/listener.go:109-133` | **M** |
| G11 | `TagValid` UID | latch, dedup, later correlate via `getTagInfo` | discarded before parsing | `internal/sselistener/listener.go:234` | **M** |
| G12 | `ContentAudioId` | radio fingerprint + ffmpeg start time | ignored | `internal/sselistener/listener.go:263` | **M** |
| G13 | Audio content | silence not enqueued; content-aware drop | content-blind, newest-chunk discard, no log | `internal/audio/recorder.go:178-183` | **M** |
| G14 | Metrics | signal vs bytes, per-subsystem | `delivered_kB_s` only, at `DEBUG` | `cmd/shim/main.go:205-212` | **H** |
| G15 | `/healthz` | reason registry, all subsystems | single string, last writer wins | `internal/server/server.go:57-61` | **M** |
| G16 | `skip_prev` | check `available_actions`, report unsupported | sent blind | `internal/sselistener/listener.go:255` | **L** |
| G17 | Attach/activate order | `activate` first, then attach writer | writer attached before `activate` is sent | `internal/soloist/supervisor.go:168-172` | **M** |
| G18 | Idle timeout value | ~30 s per the client contract | 120 s | `internal/sselistener/listener.go:43` | **L** |
| G19 | `Tilt` in docs | documented as ignored | three documents claim it is mapped to skip | `docs/DESIGN.md:154`, `docs/REQUIREMENTS.md:69`, `docs/research/teddycloud-sse-events.md:143` | **L** |
| G20 | Multi-box | documented single-box limit | not documented anywhere | — | **M** |

Two more that are not control-path but bear directly on the reported symptoms:

| # | Item | Detail | Sev |
|---|---|---|---|
| G21 | `delivered` counts bytes accepted by `bufio`, not bytes on the wire | `internal/server/stream.go:122-123` — the counter is producer-paced, so it cannot detect a stalled consumer | **M** |
| G22 | `flushStalePreFill` races the previous consumer | old and new `/stream` handlers drain the same `chunkCh`; `Flush()` (`internal/audio/recorder.go:127-142`) has no arbitration with the in-flight drain | **M** |

---

## 6. Part C — Audit of `plan_debug_sound.md` Issue 1

The plan is careful and its instrument-first instinct is right. But its central hypothesis rests on
two inferences from log lines that **the code does not emit**, and one of its conclusions is refuted
outright by the teddyCloud source.

### 6.1 The evidence is not evidence

| Plan claim | Problem |
|---|---|
| `:53` *"(no `command_result play` — no new `/stream` reached the shim)"* | `handleStream` logs **nothing** on the success path (`internal/server/stream.go:17-89`). A successful `/stream` is invisible. Further, `command_result` is only reachable at `LOG_LEVEL=debug` via `internal/soloist/supervisor.go:303` — and even then it proves nothing on its own, because SSE *also* emits a `Play("")` at the same moment. Absence of `command_result` here is **expected in the working case too**. |
| `:58` *"`sselistener: pause` never appears, so no `playback: stopped` was received"* | The log line is emitted at `INFO` before the write (`internal/sselistener/listener.go:274`), so if the event *had* arrived it *would* appear. This one inference is sound — and it points the opposite way from H1: if `playback:stopped` never arrived, then teddyCloud's own teardown path never ran either. The SSE connection was probably stale — and the idle watchdog that should have caught it is unreachable code (G9). |
| `:16` *"`position_sync` stops after the first track"* | Soloist sends `position_sync` on *anchor changes* (`docs/research/spotify-soloist.md:142`), not as a heartbeat. A quiet `position_sync` is the documented normal behaviour, not a defect. The plan's own Issue 4 is chasing a non-bug; the 30 s `get_state` poll in [§4.10](#410-observability-and-health) is the right substitute. |
| `:145` *"`ContentAudioId` fixed at `436906887`"* | For a stream, `ContentAudioId = time(NULL) - 0x50000000` and is **a fresh value on every connection** (`:1327-1360`). A constant across four placements means those four placements shared one ffmpeg run — i.e. the stream was *never* restarted. That is a real finding, and it is the opposite of "the capture shows a re-placement is a normal, complete playback cycle" (`:83-87`). |

### 6.2 H1 is refuted by the source

> **H1.** *On lift, teddyCloud emits `playback: stopped` but **keeps the existing `/stream` HTTP
> connection open**. On re-placement teddyCloud therefore does not re-request the stream.* (`:134-138`)

teddyCloud's `TBS_PLAYBACK_STOPPED` handler sets `stream_ctx.active = false`
(`:1498-1530`); `stop_on_playback_stop` is `true` for both stream paths and is **never reset**
(`:1494-1496`); that ends the ffmpeg encode loop, rewrites the TAF header, and returns the request
handler, which closes the socket (`:1594-1614`). Independently, the non-blocking send loop breaks on
`EAGAIN` as soon as the box stops pulling (`:1532-1592`). The analysis's own verdict (`:1626`):
*"teddyCloud does not keep a radio stream running for 6 hours after the tag is lifted."*

⇒ **teddyCloud cannot hold the `/stream` connection open across a lift.** On re-placement the box
must re-request content, teddyCloud must re-spawn ffmpeg, and the shim **must** see a new `GET
/stream`. Therefore either that request is arriving and something downstream is broken (H2/H3), or
the "no new `/stream`" conclusion is simply unobservable — which, given G8, it is.

The practical consequence: **the plan's proposed fix is aimed at a failure mode that the teddyCloud
source says cannot occur.**

### 6.3 The plan's cache-and-resume fix, and why it is the wrong shape

The plan proposes (`:61-79`): cache the last valid URI on `/stream`; on `TagValid`, `Play(cached)`;
delete `Play("")`; never clear the cache on `playback:stopped`; do not branch on consumer state.

Deleting the `Play("")` path is unambiguously right. The rest trades one defect for another:

| Plan's rule | Consequence |
|---|---|
| `TagValid` → `Play(cachedURI)` | Placing a **different** figurine resumes the **previous** figurine's content. If teddyCloud re-requests `/stream` (which §6.2 says it must) this is a brief wrong-content blip; if it does not, the shim plays the wrong station indefinitely. The plan's own acceptance criterion — *"a different figurine still switches content correctly"* (`:188`) — cannot be met by this design. |
| *"one code path beats a conditional one"* (`:78`) | The conditional it refuses to write — "is a consumer attached" — is the single best signal the shim has (G3). Refusing to use it is what forces the URI cache to exist at all. |
| *"do not clear the cache on `playback:stopped`"* | Moot in the ideal model: Soloist owns the context, so there is no cache to keep coherent. |
| *"the capture shows a re-placement is a normal, complete playback cycle"* (`:83-87`) | Rests on a constant `ContentAudioId` that, per `:1327-1360`, means the opposite: no new ffmpeg run. |

**The same intent, correctly shaped** is [§4.4](#44-a-real-resume-instead-of-a-guaranteed-error):
make the shim's `play` support a genuine protocol-level resume (omit `uri`), and let Soloist — the
component that actually holds the context — decide. That achieves the plan's goal ("re-placement must
not depend on teddyCloud re-requesting") without putting a stale URI in the SSE path, and it does not
require the shim to model the box's tag lifecycle at all.

### 6.4 What the plan gets right, and should keep

- **Instrument first** (`:151`). Correct, and the specific list at `:153-155` is right.
- **Issue 1 and Issue 2 must not share a commit** (`:31-34`, `:102`). Correct — but note that the
  honest fix for both is the *same* mechanism (consumer lifecycle as the liveness signal, G3), so
  sequence them as one change split across two commits at an interface boundary, not as two
  independent investigations.
- **Live edge, not buffer drain** (`:266-276`). Correct.
- **"healthy" and "mute" are indistinguishable in the logs** (`:112-120`). This is the most valuable
  observation in the document, and [§4.2](#42-liveness-three-independent-facts-each-read-from-the-right-layer)
  is the elaboration of it.
- **The one open question** (resume vs restart, `:94-101`) is the right question, asked of the right
  source. Keep it — but answer it from Soloist's `playback_state` in the *shim's own* log, which
  means G6 first.

### 6.5 The plan's Issue 2 premise that also needs retiring

`plan_debug_sound.md:245-247`: *"The shim already learns from SSE whether a figurine is on the box
(`playback: stopped` / `TagValid`). That is **exact** state, so 'should there be sound right now'
should be driven by it, not inferred."*

It is not exact state. `playback:stopped` is ambiguous (`:628-635`), `TagValid` is a placement edge
rather than a level, tag removal is invisible (`:728-737`), and a reconnect silently loses the gap
(`:2270-2273`). Intent gating is a good idea; it must be driven by the **consumer lifecycle plus
Soloist's own state**, with SSE as a hint — see [§4.2](#42-liveness-three-independent-facts-each-read-from-the-right-layer).

---

## 7. Part D — What the shim cannot fix, and what only the operator can decide

### 7.1 Latency floor

`plan_debug_sound.md:185-187` sets the acceptance bar at *"audio resumes within ~1 s of every
re-placement"*. On the teddyCloud side a single ffmpeg start costs
`ffmpeg_sweep_delay_ms` (2000) + `ffmpeg_stream_buffer_ms` (2000) before the first byte
(`:1538-1547`, `:2201-2212`). Add the box's own re-download of whatever it buffered while the tag was
up (`:1616-1621`), and a sub-second end-to-end criterion is unreachable **by construction**. The
honest target is: *no silence that the shim is responsible for*, with a stated shim-side latency
budget (buffer occupancy + `bufio` residue ≤ ~500 ms).

### 7.2 The box's stale buffer

With `encode.ffmpeg_stream_restart = false` (default), the box is told the same 240 MB total for six
hours and **resumes at its own offset** (`:1685-1708`). After a lift of 30 s the box will therefore
replay ~30 s of already-buffered radio. This is teddyCloud-side and box-side behaviour; the shim can
only avoid *adding* to it. Worth knowing before anyone reports it as a shim bug — and worth an
experiment with `ffmpeg_stream_restart = true` on a spare deployment, which trades a slower restart
for no duplicated audio (`:1700-1708`).

### 7.3 Why not MQTT

The static analysis is emphatic that MQTT is teddyCloud's control channel
(`:1855-1887`, `:1914-2040`), and that `CmdStop` is the only way to stop a stream deliberately. The
shim does not use it. That is defensible **for this deployment**, and the reasons should be written
down rather than left implicit:

- MQTT is **off by default** (`mqtt.enabled = FALSE`, `:330-344`) and needs a broker, a topic, and
  per-box common names — real operational surface for a single-box home deployment.
- The shim needs a *player*, not a *box controller*. `CmdStop`, `CmdVolLimitSppk`, `CmdLed` and
  `CmdSlapEnabled` all act on teddyCloud's own ffmpeg or on box settings
  (`:1966-1974`); none of them can make Soloist play a different track.
- The shim's control path is Soloist's WebSocket, which is **more** capable than the MQTT surface
  (`play`/`skip`/`seek`/queue, `docs/research/spotify-soloist.md:159-174`).

What is lost, and should be recorded as a limitation:

- **Per-box attribution.** No SSE event carries a box id (`:767-777`); MQTT does
  (`{topic}/box/{cn}`). **The shim's control path is single-box by construction.** A second Toniebox
  makes gesture→command attribution impossible, not merely ambiguous. This is a deployment
  constraint, not a bug, and it is currently undocumented (G20).
- **No server-side stop.** When the shim wants teddyCloud to stop pulling, it can only let go of the
  socket. That is almost always equivalent, but it is not the same thing.

### 7.4 Decisions that need the operator

1. **Soloist resume semantics** — does `{"command":"play"}` with no `uri` resume, and what does it do
   with nothing loaded? Blocks [§4.4](#44-a-real-resume-instead-of-a-guaranteed-error).
2. **Does `play <same-uri>` restart or resume in place?** Blocks the idempotence rule in
   [§4.3](#43-playbackstopped-must-not-pause-soloist).
3. **Does the box emit an audio-id report for a live stream?** ([E], `:1404-1433`) Cheap: enable
   `log.debug` and look for grp 20 fn 6803/16096, or simply read the new
   `ContentAudioId` logging. Determines how much of the radio fingerprint [§4.1](#41-identity-stream-decides-content-and-only-stream)
   is available in production.
4. **Is `skip_prev` ever offered?** [E] recorded once as absent (`plan_debug_sound.md:322-325`).
   Determines whether the backward gesture is implemented or documented as unsupported
   (`plan_debug_sound.md:337-340` is right that this is a product decision).
5. **Does teddyCloud's ffmpeg tolerate a starved `/stream` body?** The one unvalidated assumption in
   the whole silence-trickle design (`plan_debug_sound.md:241-245`). Test before relying on it.

---

## 8. Part E — Proposed work sequence

Ordered so that each step makes the next one decidable. One commit per step, per
`plan_debug_sound.md:419-421`.

| # | Change | Unblocks | Risk |
|---|---|---|---|
| **0** | **Measure** the two Soloist semantics (§7.4 items 1 and 2) against a running Soloist: send `{"type":"command","command":"play"}` with no `uri` (a) with nothing loaded, (b) while playing; send `play` with the currently-loaded uri and read `playback_state.position.position_ms`. Five minutes with a `soloist` instance and a WS client. | The whole control path | none — read-only experiment |
| **1** | `/stream` lifecycle logging: attach (`uri`, hot-swap?), detach (reason, bytes, duration), plus `ContentAudioId`/uid latching. **No behaviour change.** | Classification of Issue 1; every later step's diagnosis | none |
| **2** | Parse Soloist's WS events into a state machine; `get_state` on every (re)attach; log `playback_state` at `INFO`; periodic 30 s status line. | G6, G14, plan 1.5/1.7/4.2 | low |
| **3** | Delete `Play("")`; add the real resume form; make the `play` on `/stream` idempotent from Soloist's state. | G1, G2, G4 | low — depends on step 0 |
| **4** | Consumer-lifecycle as the liveness latch; `playback:stopped` sets a latch instead of pausing; `/healthz` reason registry. | G3, G4, G15, plan 4.3 | low |
| **5** | Command correlation + bounded pending queue + `activate`-before-attach + WS read deadline. | G7, G17 | medium |
| **6** | SSE client-contract compliance: reachable watchdog, jitter, reset on success, parse-failure logging, `TagInvalid` + raw-log2 removal. | G5, G9, G10, G11, G12 | low |
| **7** | Audio plane: silence gate + content-aware drop + split counters + periodic flush. **Only after §7.4 item 5 is measured.** | G13, G21, G22, plan Issue 2 | medium — touches real-time behaviour |
| **8** | Mock and test updates: `TagInvalid`, `tilt`, raw-log2 removal, malformed payload, 503, a >2 min idle stream; replace the test that asserts `TagInvalid` cannot exist. | makes 1–7 testable | none |
| **9** | Retire the documentation contradictions in [Appendix A](#9-appendix-a--contradictions-to-retire). | — | none |

Steps 1 and 2 are the two the plan already asks for, in the same order, and should be done first and
committed first: until they ship, another session on the box will produce the same undecidable
evidence.

---

## 9. Appendix A — Contradictions to retire

| Claim | Where | Reality |
|---|---|---|
| *"There is no `TagInvalid` event on the real server."* | `internal/sselistener/listener.go:19`; `docs/research/teddycloud-sse-events.md:71,107`; `docs/STRUCTURE.md:406,695` | It exists: `toniebox_state.c:36`, from RTNL 15/8646 (CC3200) and 15/15452 (ESP32) (`docs/teddycloud-static-analysis-sse.md:573-574`). Never *observed* on this box — a different statement. |
| A test asserting *"Must be exactly one event: playback stopped. No `TagInvalid`."* | `cmd/mock-teddycloud/main_test.go:198-206` | Encodes the wrong premise as an invariant; it will block the correct behaviour. |
| *"tilt forward/backward → skip"* | `docs/DESIGN.md:154-156`; `docs/REQUIREMENTS.md:69-70`; `docs/research/teddycloud-sse-events.md:143-145` | The implementation maps **`knock`**, and correctly ignores `tilt` (a within-track seek with no radio equivalent). Fix the three documents, not the code. |
| *"`data` is always a string"* | `docs/research/teddycloud-sse-events.md:15` | `rtnl-raw-log2`/`-log3` carry a nested object — 313 of 741 captured events (`:344-357`). |
| *"knock events are irrelevant to the control path"* | `docs/research/teddycloud-sse-events.md:115` | `knock` **is** the skip gesture; contradicted by the corrected table at `:123-124` in the same file. |
| Ear-press ordering: `pressed` → `VolumeLevel` → `VolumedB` | `docs/teddycloud-static-analysis-sse.md:942-945` | The live capture shows `VolumeLevel` → `VolumedB` → `pressed` in every instance. The wire wins. Immaterial today (all three are ignored), misleading for whoever adds volume control. |
| `44100` Hz | `docs/DESIGN.md:138`; `docs/STRUCTURE.md:257,273,276`; a stale comment at `internal/audio/recorder.go:30` contradicting `:14` in the same file | 22050, deliberately, to stay under teddyCloud's measured read ceiling (`internal/audio/audio.go:5-11`). |
| `internal/recorder/`, `FFmpegRecorder` | `docs/DESIGN.md:56,62`; `docs/STRUCTURE.md:37` | Never existed. `PulseRecorder` is `internal/audio/recorder.go`. `ChunkSource` is in `internal/server/server.go:14-17`, not `internal/audio`. |
| *"the operator runs Soloist manually with `--pair`"* | `docs/DESIGN.md:111` | Pairing is automatic (`cmd/shim/main.go:158-165`). The doc is stale. |
| `/healthz` reasons | `docs/DESIGN.md:190` | Four reasons exist, not one: `soloist_missing`, `soloist_unpaired`, `soloist_expired`, `pulseaudio_not_ready`. |
| Config table | `docs/DESIGN.md:196-205` | Missing `SOLOIST_VOLUME`, `RECORDER_BUFFER` (the knob that fixed the drops defect), `STATIC_STREAM`, `STATIC_SAMPLE_RATE`. |

---

## 10. Appendix B — Fact index

| Fact | Where to verify |
|---|---|
| teddyCloud emits no SSE for the stream it serves | `docs/teddycloud-static-analysis-sse.md:1299-1309` |
| `playback:stopped` kills ffmpeg and the HTTP response | `:1498-1530`, `:1532-1592`, `:1626` |
| Tag removal has no SSE event; the raw log2 frame is the only lossless one | `:728-737`, `:1456-1478`, `:1826` |
| `playback:stopped` is ambiguous (system sounds) | `:628-635` |
| `ContentAudioId` is a radio fingerprint carrying the ffmpeg start time | `:1327-1360` |
| `playback:starting` falls through to `started`; carries no information | `:620-627` |
| No SSE event carries a box id | `:767-777` |
| No `id:`/`retry:`/replay ⇒ level-triggered clients required | `:358-367`, `:2270-2273` |
| Payloads are not JSON-escaped | `:315-342`, `:2493-2510` |
| No REST readback for tag/stream live state | `:2244-2247` |
| ffmpeg start latency ≥ 2 s | `:1538-1547`, `:2201-2212` |
| Box resumes at its own offset; ~240 MB promised | `:1685-1716` |
| Only `MQTT CmdStop` stops a stream deliberately | `:1839-1842`, `:1966-1974` |
| `play`'s `uri` is optional; `"uri":""` is rejected | `docs/research/spotify-soloist.md:163`; `plan_debug_sound.md:49,52` |
| `command_result` = accepted, not applied | `docs/research/spotify-soloist.md:147` |
| `position_sync` is an anchor, not a heartbeat | `docs/research/spotify-soloist.md:142` |
| Soloist event catalogue and query commands | `docs/research/spotify-soloist.md:130-174` |
| Shim's only `/stream` log line is a failure warning | `internal/server/stream.go:74` |
| Shim discards every Soloist event | `internal/soloist/supervisor.go:303` |
| Shim's `play` sends `"uri":""` | `internal/soloist/connector.go:40-44` |
| `TagValid` → `Play("")`, UID discarded | `internal/sselistener/listener.go:234` |
| `playback:stopped` → `Pause()` | `internal/sselistener/listener.go:238` |
| SSE watchdog unreachable (blocking `Scan()`), 2 min value | `internal/sselistener/listener.go:41-43,181-193` |
| Content-blind drop policy | `internal/audio/recorder.go:178-183` |
| 128 KiB `bufio` ≈ 743 ms latency residue | `internal/server/stream.go:100-111` |
| Telemetry is one `DEBUG` line, bytes only | `cmd/shim/main.go:205-212` |
| `/healthz` is a single last-writer-wins string | `internal/server/server.go:57-61` |
| `DELIVERED` counts `bufio` accepts, not wire bytes | `internal/server/stream.go:122-123` |
