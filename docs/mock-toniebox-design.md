# `cmd/mock-toniebox` — design

**Date:** 2026-10-03 (design), 2026-10-08 (implemented, §16 records what running it changed)
**Status:** implemented and verified against a live teddyCloud. See §16 for the findings that
only appeared once the mock was actually run, and §15 for what is still open.
**Purpose:** make [`docs/ideal-shim-behavior.md`](ideal-shim-behavior.md) implementable and testable
against real teddyCloud with **no physical Toniebox**.
**Extends:** [`spike_mock_toniebox.md`](spike_mock_toniebox.md) (a feasibility spike — this
document turns it into a tool, and corrects two of its conclusions).
**Protocol source of truth:** [`docs/teddycloud-static-analysis-sse.md`](teddycloud-static-analysis-sse.md),
plus a fresh read of teddyCloud v0.7.0 `78c3b38` at `/var/home/jan/ws/teddycloud`.

Claim tags follow `ideal-shim-behavior.md`: **[P]** proven from source, **[E]** empirical/firmware,
**[H]** hypothesis needing a measurement.

---

## Table of contents

1. [Why a box mock at all](#1-why-a-box-mock-at-all)
2. [Position in the test rig](#2-position-in-the-test-rig)
3. [Two source findings that change the design](#3-two-source-findings-that-change-the-design)
4. [Architecture](#4-architecture)
5. [Box identity and transport](#5-box-identity-and-transport)
6. [RTNL wire specification](#6-rtnl-wire-specification)
7. [Content consumption](#7-content-consumption)
8. [Switchable box-behaviour assumptions](#8-switchable-box-behaviour-assumptions)
9. [Control surface](#9-control-surface)
10. [Observation and assertions](#10-observation-and-assertions)
11. [Scenarios mapped to Part E](#11-scenarios-mapped-to-part-e)
12. [What the mock cannot prove](#12-what-the-mock-cannot-prove)
13. [The Soloist gap](#13-the-soloist-gap)
14. [Implementation layout](#14-implementation-layout)
15. [Open questions](#15-open-questions)
16. [What running the mock changed](#16-what-running-the-mock-changed)

---

## 1. Why a box mock at all

`plan_debug_sound.md` Issue 1 — *"audio is silent for 10-20 s after replacing a figurine"* — is
undecidable with the current harness because nothing observes `/stream` from the outside.
`cmd/mock-teddycloud` emits SSE events on demand, so it can drive the shim's control path, but it
cannot produce the **data path**: nothing ever calls `GET /stream`, so teddyCloud never runs ffmpeg,
so no TAF exists and no re-placement behaviour can be observed.

The Toniebox is the component that closes that loop. It is the only actor that:

1. emits RTNL placement/removal/gesture frames, which teddyCloud translates into SSE, and
2. issues `GET /v2/content` / `GET /v3/content-meta` + `/v3/chapter`, which makes teddyCloud's ffmpeg
   pull `GET /stream` from the shim.

Mock the box and the whole chain runs for real:

```
                  ┌─────────────── no hardware anywhere ───────────────┐
                  │                                                │
  mock-toniebox ──┼─ RTNL *binary ──▶ teddyCloud ── GET /stream ──▶ shim ── WS ──▶ Soloist
        ▲         │                        │                              │
        │         │                        └── GET /v1/freshness-check ◀─┤ (mock asks)
        │         │                            GET /v3/content-meta ◀────┤ (mock asks)
        │         │                            GET /v3/chapter      ◀────┤ (mock asks)
        │         │                                                            │
        └─────────┴──────────────── SSE observer (/api/sse) ◀──────────────────┘
```

The shim is the code under test and is untouched. teddyCloud performs the real RTNL→SSE translation
and the real ffmpeg pull, so the shim is exercised under genuine traffic rather than a synthetic
stream.

---

## 2. Position in the test rig

Three tools, three directions. They do not overlap.

| Tool | Mocks | Drives | Use for |
|---|---|---|---|
| `cmd/mock-teddycloud` (exists) | the teddyCloud **SSE server** | the shim's control path | SSE-contract tests: `TagInvalid`, `tilt`, raw-log2 removal, malformed payloads, 503, >2 min idle. Part E steps 6 and 8. |
| `cmd/mock-toniebox` (this) | the Toniebox | **real teddyCloud** → the shim | End-to-end: `/stream` attach/detach, format, re-placement timing, `ContentAudioId` correlation. Part E steps 1, 4, 8. |
| `mock-soloist` (not yet written) | the Soloist **WebSocket server** | the shim's producer path | Soloist state-machine tests without Premium or pairing. Part E steps 2, 3, 5. |

**This tool does not replace `cmd/mock-teddycloud`.** Hostile and malformed SSE is cheaper and more
precise to generate directly; a mock box cannot produce it, because teddyCloud is in the path and
teddyCloud emits well-formed SSE. Part E step 6 stays on the existing mock.

The spike's own framing — *"a verification harness on the consumption side only"* — is kept. The
Spotify capture side is the shim's problem and is not modelled.

---

## 3. Two source findings that change the design

Both were found by reading teddyCloud after the spike was written. Both invalidate part of it.

### 3.1 `playback:starting` always also emits `playback:started`

`src/toniebox_state.c:73-79` handles log3 `field2 == 11` (`PLAYBACK_STARTING`) and then **falls
through** into the `field2 == 12` (`PLAYBACK_STARTED`) case — there is no `break`. A box that sends
`11` produces both SSE events in one batch. **[P]**

`ideal-shim-behavior.md` Appendix B already records that `starting` "carries no information". The mock
makes it concrete: **there is no way to emit `starting` alone.** The shim must be idempotent about the
pair, and the mock must not be written to expect otherwise.

### 3.2 Freshness-check always marks a stream as needing refresh

`process_freshness_check` (`src/handler_cloud.c:1031-1034`) includes a requested UID in
`tonie_marked` when:

```c
json.live || updated || _source_type in {CT_SOURCE_STREAM, CT_SOURCE_TAP_STREAM} || uid == flex_uid
```

For this project `_source_type` is always `CT_SOURCE_STREAM` (content.json `source:` points at the
shim). The `CT_SOURCE_STREAM` disjunct is therefore **unconditionally true**, so the UID is always
marked, regardless of the audio id the box reports. **[P]**

Consequence: `plan_debug_sound.md` Step 2's hypothesis — *"same cached audio id ⇒ teddyCloud does not
re-request the content, so no new `/stream`"* — is **refuted by source.** A re-placement will always
produce a fresh content request with `Range: bytes=0-`, hence a fresh `GET /stream`, hence a fresh
`play`. That is the correct baseline for the shim's idempotency work, and it is not a bug.

**So the freshness audio-id knob is not the interesting lever.** The levers that actually change shim
behaviour are:

- the **Range offset** the box requests with (`bytes=0-` vs `bytes=N-`), and
- the **read cadence** at which the box drains the content body.

Those are what §8 makes switchable, and §11 turns into assertions.

---

## 4. Architecture

Single process, three goroutine groups:

| Goroutine | Responsibility |
|---|---|
| RTNL writer | owns the TLS connection to teddyCloud; serialises frames; reconnects with backoff |
| Downloader | one at a time; runs the placement handshake and drains the content body at the configured rate |
| Observer | connects to `GET /api/sse` on the same teddyCloud; timestamps every event; feeds the assertion engine |

Plus a control HTTP listener (`POST /control/...`, `GET /`) so the mock can be driven by curl, by a
browser, or by a shell script — mirroring `cmd/mock-teddycloud`'s ergonomics.

A single `box` struct owns all mutable state (cached audio id, downloaded bytes, current UID, firmware
profile) behind one mutex. No state is global.

---

## 5. Box identity and transport

### 5.1 Certificate and overlay id

teddyCloud never validates the client certificate beyond a substring check, and the strings it
matches against are **not** the whole distinguished name.

`src/tls_adapter.c:626-627` copies only two fields into the TLS context, both of them the
common name:

```c
copyAsString(context->client_cert_issuer, ..., certInfo->tbsCert.issuer.commonName.length, ...);
copyAsString(context->client_cert_subject, ..., certInfo->tbsCert.subject.commonName.length, ...);
```

So `O=`, `C=` and every other RDN are invisible to the gate. The gate itself
(`src/server.c:320-321`) is:

```c
osStrstr(issuer, "Boxine Factory SubCA") || osStrstr(issuer, "Toniebox SubCA")
    || osStrstr(issuer, "TeddyCloud")     || osStrstr(subject, "TeddyCloud")
    || osStrstr(issuer, "Toniebox Root CA")
```

and it only runs at all when `connection->tlsContext && connection->private.api_access_only`
(`src/server.c:314`). Failure means `authenticated` stays false, and on the HTTPS API listener
`core.boxCertAuth` defaults to `TRUE` (`src/settings.c:139`), so `src/server.c:516-520` answers
every request `401`. The code carries its own `// TODO: CHECK THE CERTIFICATES FOR REAL!!!!!` at
`:349`. **[P]**

**Consequence — the design's first draft was wrong here.** A self-signed leaf carrying
`O=TeddyCloud` does **not** pass, because a self-signed certificate's issuer CN equals its own
subject CN, and neither contains the substring `TeddyCloud`. What is required is a two-level chain:

| Certificate | Subject CN | Role |
|---|---|---|
| throwaway CA | `TeddyCloud` | satisfies `osStrstr(issuer, "TeddyCloud")` |
| leaf | `<12 hex MAC>` | satisfies the 12-character CN branch |

The gate only ever sees the two common names, so the CA's is the only place the magic substring can
live. **[P]**

Once past the gate, identity comes from the subject CN (`src/server.c:314-359`):

| Subject form | CN extracted |
|---|---|
| length 15, starts `b'`, ends `'` | the 12 inner characters |
| length exactly 12 | the subject |
| anything else | `commonName` stays NULL, and `get_settings_cn(subject)` runs on the raw subject instead |

`get_settings_cn()` then uses the **sanitised** CN as both `commonName` and
`internal.overlayUniqueId`, and names the box `teddyCloud Box <CN>` (`src/settings.c:459-485`).
Per-overlay RTNL state — including `internal.rtnl.lastEarpress`, `wasDoubleEarpress` and
`multipressTime` (`src/settings.c:200-202`) — the freshness cache, and
`internal.toniebox_firmware.*` are all keyed on it. **[P]**

Both subject forms therefore work. The mock uses the plain 12-character form, `CN = <MAC>`, because
`--mac` is already twelve hex characters and the 15-character `b'[...]'` spelling adds nothing once
the CA is separate. `--uid` (the figurine NFC UID, 16 hex) and `--mac` (the box MAC, 12 hex) are
separate knobs and must not be confused; the default MAC is fixed so runs are reproducible.

`core.allowNewBox` defaults to `TRUE` and `toniebox.api_access` to `TRUE`
(`src/settings.c:139-140`, `:301`), so an unknown CN is accepted without pre-registration. **[P]**

### 5.2 TLS and the two listeners

teddyCloud runs two listeners with two different request callbacks
(`src/server.c:915` and `:935`):

| Listener | Port | `is_api_only` | Serves |
|---|---|---|---|
| web | `core.server.http_port` = 80 | `false` | `SERTY_WEB` and `SERTY_BOTH` routes, plus `/` and the GUI |
| API | `core.server.https_api_port` = 443 | `true` | `SERTY_API` and `SERTY_BOTH` routes |

**No route is `SERTY_API`-only** — every box route (`*binary`, `/v1/*`, `/v3/*`) is `SERTY_BOTH`
(`src/server.c:106,173,177,181-182`), so both listeners accept box traffic. **[P]** Two things
still differ between them:

1. Only the API listener enters the certificate gate at all (`src/server.c:314`), so only there
   does the leaf CN become `client_ctx->settings`. On the web listener `get_settings()` returns the
   global settings and **every per-overlay key falls back to the default overlay**. Two runs that
   must not share ear-press or freshness state need the HTTPS listener.
2. `/api/sse` is `SERTY_WEB` (`src/server.c` route table) and therefore exists **only on the web
   listener**. The observer cannot share `--teddycloud`; it takes `--sse`, which defaults to
   `http://<teddycloud host>:80`. **[P]**

`TLS_CLIENT_AUTH_OPTIONAL` — a client cert is never required at the TLS layer
(`src/server.c:785-786`), and the trusted-CA list is never installed (`:794-795`, the condition is
literally `1 == 0`). The mock connects with `InsecureSkipVerify: true` and supplies the client
cert. **[P]**

SNI selects the server cert (`src/server.c:718-725`): SNI present → TB2 chain, absent → TB1 chain.
`--sni` is therefore a boolean — off (default) exercises TB1, on exercises TB2. SNI only changes
which certificate teddyCloud presents; it never changes where the mock connects. **[P]**

Plain HTTP is selected by giving `--teddycloud` an `http://` URL. The RTNL detection lives in
shared code (`src/cyclone/cyclone_tcp/http/http_server.c:474-494`), not in the TLS path, so the
binary stream works on the web listener too; the mock logs a WARN because of the overlay fallback
above. **[P]**

`checkSecMitHandlers` runs before the route table but only enforces host-hash blacklists and crawler
user-agents (`src/handler_security_mit.c:58-79`, `:124-154`). The mock sends a Toniebox-like
`User-Agent` and a `Host` that is not on the blacklist. **[P]**

---

## 6. RTNL wire specification

### 6.1 How teddyCloud decides a request is RTNL

`httpReadRequestHeader()` reads with `SOCKET_FLAG_BREAK_CRLF`
(`src/cyclone/cyclone_tcp/http/http_server_misc.c:80-81`), so the **first read stops at the first
`0x0D`/`0x0A` in the byte stream**. teddyCloud then treats the request as binary RTNL if
(`src/cyclone/cyclone_tcp/http/http_server.c:474-494`):

1. `httpParseRequestLine()` fails with `ERROR_INVALID_REQUEST`,
2. `contentLength > 4`, and
3. `buffer[0] == 0 && buffer[1] == 0`.

So the first write must be **raw binary beginning `00 00`** and **longer than 4 bytes**, and must
never begin with a parseable HTTP request line. Every RTNL frame satisfies this. **[P]**

The connection is then kept open in a loop that PEEKs for more data, sleeping 100 ms when empty,
until the peek errors, then falls back to header-read mode with a 5-minute idle timeout. Ordinary
HTTP requests may follow on the same TLS connection. **[P]**

Constraint: the connection buffer is 32 KiB (`include/net_config.h:230`) and `protoLength` must be
`< 65536` (`src/handler_rtnl.c:111`). Frames here are ~15 bytes, so this is not close to binding.

### 6.2 Framing

`src/handler_rtnl.c:85-148`:

```
[uint32 big-endian payload length][protobuf payload]
```

| Rule | Behaviour | Line |
|---|---|---|
| Length decode | `(buf[0]<<24)\|(buf[1]<<16)\|(buf[2]<<8)\|buf[3]` | `:102` |
| `protoLength == 0` | `ERROR_FAILURE`, connection dropped | `:111-115` |
| `buf[pos] != 0 \|\| buf[pos+1] != 0` | `ERROR_FAILURE` — an undocumented 64 KB ceiling | `:111-115` |
| Partial frame | keep the remainder for the next call (`memmove` to front) | `:98-107`, `:144-145` |
| Multiple frames per read | processed in order | `:95-141` |
| Unpack failure, or neither `log2` nor `log3` | **silently dropped**, no log | `:131-139` |

Every frame write also emits an MQTT `LastSeen` for the box (`src/handler_rtnl.c:90-92`) — a useful
liveness side effect the mock can assert on.

### 6.3 Messages

All proto2, small enough to hand-encode. **No `protoc`, no protobuf dependency.**

`proto/toniebox.pb.rtnl.proto`:

| Message | Field | # | Wire type | Tag byte | Required |
|---|---|---|---|---|---|
| `TonieRtnlRPC` | `log2` | 2 | LEN | `0x12` | optional |
| | `log3` | 3 | LEN | `0x1A` | optional |
| `TonieRtnlLog2` | `uptime` | 1 | fixed64 | `0x09` | **yes** |
| | `sequence` | 2 | uint32 | `0x10` | **yes** |
| | `field3` | 3 | uint32 | `0x18` | **yes** |
| | `function_group` | 4 | uint32 | `0x20` | **yes** |
| | `function` | 5 | uint32 | `0x28` | **yes** |
| | `field6` | 6 | bytes | `0x32` | **yes** |
| | `field8` | 8 | fixed32 | `0x45` | optional |
| | `field9` | 9 | bytes | `0x4A` | optional |
| `TonieRtnlLog3` | `datetime` | 1 | fixed32 | `0x0D` | **yes** |
| | `field2` | 2 | uint32 | `0x10` | **yes** |

Encoding rules that matter, all from the bundled protobuf-c:

- required fields are **always emitted**, even when zero (`src/proto/protobuf-c.c:1973-1974`);
- a missing required field makes `__unpack` return `NULL`, so the frame is **silently dropped**
  (`:3258-3268`) — an encoder bug presents as "teddyCloud ignored me", with nothing in the log;
- **fixed32 and fixed64 are little-endian on the wire** (`:934-958`);
- varints are base-128 little-endian; `bytes` fields carry a varint length prefix (`:1626-1634`).

There are **no length checks on `field6`** in any branch
(`src/handler_rtnl.c:336-492`), so a short payload causes an out-of-bounds read. The encoder must
emit the exact sizes in §6.4/§6.5.

### 6.4 `log3` — event type

`src/handler_rtnl.c:262-330`:

| `field2` | Name | Effect |
|---|---|---|
| 1 | `EAR_BIG` | SSE `pressed=ear-big`; or `ear-big-double` if the double-press window is armed |
| 2 | `EAR_SMALL` | same, small ear |
| 3 | `KNOCK_FORWARD` | SSE `knock=forward` |
| 4 | `KNOCK_BACKWARD` | SSE `knock=backward` |
| 5 | `TILT_FORWARD` | SSE `tilt=forward` |
| 6 | `TILT_BACKWARD` | SSE `tilt=backward` |
| 7 | `CHARGER_ON` | SSE `charger=on` |
| 8 | `CHARGER_OFF` | SSE `charger=off` |
| 11 | `PLAYBACK_STARTING` | SSE `playback=starting` **then falls through to 12** (§3.1) |
| 12 | `PLAYBACK_STARTED` | SSE `playback=started` |
| 13 | `PLAYBACK_STOPPED` | stops the active stream; clears `tag.uid/valid/audio_id`; SSE `playback=stopped`; empties `TagValid`/`ContentAudioId`/`ContentTitle` (`src/toniebox_state.c:80-97`) |

`playback=stopped` **wipes the box's tag state**, so the mock must re-send the placement frame after
a stop or the next download has no UID. The mock's state machine handles this; a naive request script
would not.

### 6.5 `log2` — `(function_group, function)`

Groups: `3=UNKNOWN3, 6=NETWORK_HTTP, 8=FIRMWARE, 12=TILT, 15=TAG, 20=AUDIO_A, 22=AUDIO_B,
27=VOLUME, 37=NETWORK_TCP` (`include/handler_rtnl.h:16-27`).

| Group | Function | `field6` layout | Effect | Line |
|---|---|---|---|---|
| 15 TAG | 8627 / 16065 | ≥8 B, **big-endian uint64** UID | `tbs_tag_placed(uid, true)`, SSE `TagValid` (16-hex) | `:336-350` |
| | 8630 / 16067 | as above | `tbs_tag_removed(uid, true)` — **no SSE**; only the raw log2 frame records it | `:336-350` |
| | 8646 / 15452 | as above | `tbs_tag_placed(uid, false)`, SSE `TagInvalid` | `:336-350` |
| 20 / 22 AUDIO | 6803 / 16096 (grp 20), 297 (grp 22) | ≥4 B, **little-endian uint32** at offset 0 | `tag.audio_id`, SSE `ContentAudioId` | `:351-400` |
| 12 TILT | 15426 / 15427 | ≥4 B, LE int32 angle | SSE `BoxTilt-A` / `BoxTilt-B` | `:401-417` |
| 27 VOLUME | 8672 / 15524 | **12 B**: bytes 0-3 ignored, LE int32 dB @4, LE int32 level @8 | SSE `VolumeLevel`, `VolumedB`; **arms the double-press window** | `:418-439` |
| 8 FIRMWARE | 704 / 703 / 7146 | NUL-terminated string (7146 at offset 8) | `internal.toniebox_firmware.*` | `:440-458` |
| 6 / 37 NETWORK | 791 / 1009 | NUL-terminated string | `internal.rtnl.*` | `:459-482` |
| 22 AUDIO_B | 6480 | NUL-terminated `content/<8 hex>/<8 hex>` | `tbs_playback_file()` — **only system sounds**; any other path is silently ignored (`src/toniebox_state.c:104-139`) | `:483-491` |

The 8627/16065 etc. pairs are per-chip: **CC3200 uses 8627/8630/8646, ESP32 uses 16065/16067/15452**.
`--firmware` selects the set.

### 6.6 The double-press ordering trap

The window check runs on the **volume** frame, never on the press itself
(`src/handler_rtnl.c:433-437`):

```c
if (uptime - lastEarpress < multipressTime) wasDoubleEarpress = true;
lastEarpress = uptime;      /* group 27 only */
```

and the press handler (`:262-295`) only looks at the flag:

```c
if (lastEarId == EAR_BIG && wasDoubleEarpress) { /* ear-big-double; clears both */ }
else                                           { /* ear-big; sets lastEarId only */ }
```

`multipressTime` defaults to **300 ms**, per overlay (`internal.rtnl`,
`src/settings.c:202`). Four consequences the mock has to respect:

1. **`log3` is dispatched before `log2` inside one `rtnlEvent()` call** (`:262` then `:332`), so a
   volume frame sharing an RPC with the press arrives too late to arm anything. Volume and press
   are always separate frames. **[P]**
2. **A double is four frames, not three:** `volume, press, volume, press`, with the *two volume
   frames* less than 300 ms apart. Two presses with only one volume frame never qualify — nothing
   in the log3 path writes `wasDoubleEarpress`. **[P]**
3. **A single press needs the opposite.** The first volume frame must be *outside* the window of
   the mock's previous volume frame, otherwise it arms the flag and — if `lastEarId` already equals
   that ear — the press is reported as a double. The mock therefore spaces its own volume frames
   `MultiPressWindow` apart unless it is deliberately producing a double. **[P]**
4. **The subtraction is unsigned `uint64`.** A constant `uptime` makes every repeat press register
   as a double; a `uptime` that goes backwards wraps to a huge value and never doubles. The mock
   keeps `uptime` strictly monotonic in milliseconds since start, so the real 300 ms window applies.

Residual caveat the mock cannot remove: `wasDoubleEarpress` is **never cleared by the volume
handler** — only a successful double detection sets it back to `FALSE` (`:271`). If a previous run
left it `TRUE`, the first same-side press reads as a double regardless of what the mock sends. Using
a fresh `--mac` (a fresh overlay starts at the `FALSE` default, `src/settings.c:201`) or performing
one deliberate `/control/ear/<side>/double` restores a known state. **[P]**

`sequence` and `field3` are never validated (`:216-218`, `:501-503`), and there is no staleness
rejection anywhere. The mock still sends plausible values so that teddyCloud's logs and MQTT read
correctly during debugging.

### 6.7 Worked frames

Playback starting (log3 `field2=11`, `datetime=0`):

```
00 00 00 09 1A 07 0D 00 00 00 00 10 0B
└ len 9 ┘ RPC log3 datetime field2
```

Tag placed, uid `fa9c3e0d500304e0` (its rUID is `E00403500D3E9CFA` — the two are byte-reversed),
CC3200 (grp 15 fn 8627), `uptime`/`sequence` zero:

```
00 00 00 1E                                  frame length 30
  12 1C                                      RPC.log2, len 28
  09 00 00 00 00 00 00 00 00                 uptime  (fixed64 LE)
  10 00                                      sequence
  18 00                                      field3
  20 0F                                      function_group = 15
  28 B3 43                                   function = 8627  (varint, not 3B 21)
  32 08 FA 9C 3E 0D 50 03 04 E0              field6 = BE uint64 uid
```

`internal/toniebox/rtnl_test.go` pins both of these as golden bytes. An earlier draft of this
section had `12 18` and `28 3B 21`: 8627 is a two-byte varint (`B3 43`, because 8627 = 67·128 + 51)
and `field3` is a required field that is always emitted, so the log2 body is 28 bytes and the frame
is 30. A hand-computed frame that is off by two bytes fails the same way a broken encoder does —
teddyCloud silently drops it.

---

## 7. Content consumption

### 7.1 Routes

| Method | Path | Auth | Used when |
|---|---|---|---|
| POST | `/v1/freshness-check` | none | `--freshness v1` |
| POST | `/v3/freshness-check` | none | `--freshness v3` |
| GET | `/v2/content/<ruid>` | required | `--content-path v2` |
| GET | `/v3/content-meta/<ruid>` | required | `--content-path v3` |
| GET | `/v3/chapter/teddycloud_NN_<ruid>` | required | `--content-path v3` |

Auth on `/v2/content` and `/v3/*` requires an `Authorization: BD <64 hex>` or `Bearer <64 hex>`
header **that parses as digest mode**, or the handler returns without responding
(`src/handler_cloud.c:879-887`). teddyCloud never verifies the value
(`httpServerAuthCallback` returns `HTTP_ACCESS_ALLOWED` unconditionally, `src/server.c:661-663`), so
the mock sends a fixed 64-hex placeholder. `/v1/*` never checks.

rUID extraction is by fixed byte offset and requires exactly 16 hex characters: `uri[12..27]` for
`/v1` and `/v2`, `uri[17..32]` for `/v3/content-meta` (`src/handler_cloud.c:868`, `:1398`). Anything
short returns `ERROR_NOT_FOUND`. **The mock constructs these offsets rather than slicing by index in
its own code**, and a unit test asserts the offsets.

The UID is byte-reversed between the RTNL `field6` bytes and the rUID URL text:
`uid = bswap_64(strtoull(ruid, 16))` (`src/handler_cloud.c:489-490`). The RTNL `field6` bytes and the
rUID hex string are the same order; the *integer* is the reverse. Getting this backwards yields a
404 with a valid-looking UID — a trap worth a dedicated unit test.

### 7.2 Freshness-check

`POST /v1/freshness-check`, request (`proto/toniebox.pb.freshness-check.fc-request.proto`):

| Message | Field | # | Type |
|---|---|---|---|
| `TonieFreshnessCheckRequest` | `tonie_infos` | 1 | repeated `TonieFCInfo` |
| `TonieFCInfo` | `uid` | 1 | required fixed64 |
| | `audio_id` | 2 | required fixed32 |

Two transport constraints (`src/handler_cloud.c:1050-1056`): the body must be **under 4096 bytes**
or teddyCloud logs an error and sends **no response at all**, and the read is a single
`httpReceive` without `WAIT_ALL`, so the body should arrive in **one TLS record**.

Response (`proto/toniebox.pb.freshness-check.fc-response.proto`): `tonie_marked` is
`repeated fixed64`; `field2`, `max_vol_spk`, `slap_en`, `slap_dir`, `field6`, `max_vol_hdp`, `led`
are all required `int32`, so they are always on the wire even at zero. The mock **does not need to
parse them** — it only needs `tonie_marked` — but the parser is implemented and unit-tested so that
a future consumer can read the box-setting fields.

The **audio-id trap** from the spike §2 still applies to the value the mock *reports*: store exactly
the audio id from the TAF header that was downloaded (that is what hardware does, reproducing
"already current") and send `0` for an empty cache. Per §3.2 this does not change the outcome for
`CT_SOURCE_STREAM`, but the knob stays because it becomes decisive the moment the content source
changes. Note the comparison direction is counter-intuitive: `updated` is true when the **box's** id
is *older* than the server's (`src/handler_cloud.c:963-977`).

`--freshness v3` posts `{"content":{"<rUID>": <audioId>}}` and reads `{"items":["<rUID>", ...]}`.
Body must also be under 4096 or the connection is torn down (`:1126-1130`).

### 7.3 Range handling — the primary lever

teddyCloud parses Range with `sscanf(value, "bytes=%u-%u", &start, &end)`, matched
case-insensitively (`src/cyclone/cyclone_tcp/http/http_server_misc.c:454-462`), and applies it
**only if `start > 0`** (`src/cyclone/cyclone_tcp/http/http_server.c:1092-1110`).

| Request | Result |
|---|---|
| no Range | `200`, full `Content-Length` |
| `Range: bytes=0-` | `start == 0` ⇒ **ignored**, `200` full body |
| `Range: bytes=N-` | `end == 0` clamps to `size-1` ⇒ `206`, `Content-Range: bytes N-(size-1)/size` |
| `Range: bytes=N-M` | `206`, `Content-Range: bytes N-M/size`; `M >= size` clamps |
| `bytes=-N`, multi-range | rejected / ignored |

`Range.start != 0` makes ffmpeg **append** to the existing partial stream TAF and reuse its audio id
(`src/handler_cloud.c:645`, `src/toniefile.c:119-135`). There is no `416` — the source carries
`// TODO add status 416 on invalid ranges` (`http_server.c:1091`).

This is the single most important behaviour the mock controls, because it is the difference between
"teddyCloud starts a fresh ffmpeg and the shim gets a new `/stream`" and "teddyCloud continues
writing to the existing TAF and the shim gets nothing". Both are reachable from the mock by choosing
`bytes=0-` vs `bytes=N-`.

### 7.4 Read cadence — the second lever

A stream advertises `Content-Length = 251658239` (240 MiB − 1) with `ffmpeg_stream_restart = FALSE`
(`src/handler_cloud.c:638-688`, `src/settings.c:288-291`), and teddyCloud waits
`ffmpeg_sweep_startup_buffer` + `ffmpeg_sweep_delay_ms` 2000 ms, then `ffmpeg_stream_buffer_ms`
2000 ms before it even sends headers.

The box therefore must consume **at roughly real time** — a box that drains instantly is not a box.
`--speed` makes the rate explicit:

| `--speed` | Emulates |
|---|---|
| `1.0` (default) | a box playing normally |
| `0.5` | a box whose speaker is struggling / a slow link |
| `0` | no pacing at all — see the correction below |
| `8` | a fast consumer, draining faster than ffmpeg produces |

For Opus at 48 kHz stereo (§11.2 of the spike) the payload rate is roughly 32 KiB/s, so a 240 MiB
body is ~2 h of audio — the mock never reaches the end during a test. `--speed` is therefore a rate
limiter on the read loop, not a completion target.

**Correction from §16:** this table originally gave `0` as "a stalled consumer". It is not, and
cannot be. The pacer gates *how fast* the loop reads; a rate of zero means "do not gate", which is
the opposite — an unlimited drain. A consumer that stops reading while holding its connection open
is modelled by the download gate instead (`POST /control/stall`), because that is a different
machine: it leaves teddyCloud with an open connection and a body that has stopped advancing, which
is the state §11.1-3 is about. `--speed 0` now means "no pacing", and the two conditions are
distinguishable in a report rather than collapsed into one flag.

### 7.5 What the mock records

Per download: UID, path used, `Range` header sent, status code, `Content-Length` advertised, bytes
received, wall duration, measured bytes/s, first-byte latency, and the audio id parsed out of the
downloaded TAF header. This is the raw material for §10.

---

## 8. Switchable box-behaviour assumptions

**This is the core design principle.** The mock encodes *assumptions* about firmware. If each one is
baked in as fixed behaviour, the tool silently becomes a machine that proves the assumption. Every
assumption is therefore a named flag, and the flag values in use are printed in the startup banner and
included in every assertion report, so a report is always attributable to a specific set of
assumptions.

### 8.1 Identity and firmware

| Flag | Values | Default | Assumption it encodes |
|---|---|---|---|
| `--firmware` | `cc3200`, `esp32` | `cc3200` | which chip, hence which tag/volume function codes |
| `--sni` | boolean | off | off = no SNI = TB1 chain; on = SNI = TB2 chain (`src/server.c:718-725`) |
| `--mac` | 12 hex | `00aabbccdd00` | the box MAC → leaf CN → overlay id; keeps runs reproducible |
| `--uid` | 16 hex | `e00403500eea4bf2` | the figurine NFC UID |

### 8.2 Content path

| Flag | Values | Default | Assumption |
|---|---|---|---|
| `--content-path` | `v2`, `v3` | `v3` | TB1 firmware uses `/v2/content`; TB2 uses `/v3/*` |
| `--fallback` | `off`, `on-404`, `always` | `on-404` | whether and when a box falls back to the other path |
| `--freshness` | `v1`, `v3`, `none` | `v1` | which freshness endpoint firmware uses |
| `--report-audio-id` | `taf`, `zero`, `stale`, `omit` | `taf` | what the box claims to have cached (the spike §2 trap) |
| `--recheck` | boolean | on | whether the box freshness-checks on every placement |

### 8.3 Download mechanics — the levers that matter

| Flag | Values | Default | Assumption |
|---|---|---|---|
| `--range-offset` | `zero`, `resume` | `zero` | a fresh placement always re-requests from 0; `resume` continues from the last byte received |
| `--speed` | float ≥ 0 | `1.0` | consumption rate, × real time |
| `--stall-ms` | ms | `0` | how long to stop reading mid-body without disconnecting |
| `--drop-after` | bytes | `0` (off) | abort the body after N bytes; with `--range-offset=resume` the placement then issues `Range: bytes=N-` |
| `--max-download` | bytes | `0` (unlimited) | stop after N bytes and go idle, holding the connection open |

`--range-offset resume` plus `--drop-after` is the combination that exercises teddyCloud's append
path (`src/handler_cloud.c:645`) and therefore the *difference* between "consumer went away" and
"consumer came back".

### 8.4 Telemetry realism

| Flag | Values | Default | Assumption |
|---|---|---|---|
| telemetry | `POST /control/telemetry` | — | grp 6/8/37 informational frames (firmware version, region, RTNL/prod domains). Sent on demand rather than on a timer, because the values are static for a given run and a timer would only add noise to teddyCloud's logs. |
| `--pad-field6` | bytes | `0` | extra trailing bytes in `field6`, to prove teddyCloud's readers tolerate a longer payload |
| `--double-press-ms` | ms | `150` | the gap between the two volume frames of a double press. Must stay under teddyCloud's 300 ms `multipressTime` or `wasDoubleEarpress` is never set (§6.6). |

---

## 9. Control surface

Mirrors `cmd/mock-teddycloud`: an HTTP listener plus a minimal HTML page, so it is equally drivable by
a person and by a script.

| Endpoint | Effect |
|---|---|
| `POST /control/place?uid=<hex>` | full placement: tag frame → freshness-check → content download |
| `POST /control/lift?uid=<hex>` | volume + `playback=stopped`, then the tag-removed frame (grp 15, fn 8630/16067) |
| `POST /control/invalid?uid=<hex>` | tag-invalid frame (8646/15452) |
| `POST /control/playback/starting\|started\|stopped` | raw log3 playback frames |
| `POST /control/knock/forward\|backward` | log3 3 / 4 |
| `POST /control/tilt/forward\|backward` | log3 5 / 6 |
| `POST /control/charger/on\|off` | log3 7 / 8 |
| `POST /control/ear/big\|small` | volume frame then press frame, spaced out of the window → single press |
| `POST /control/ear/big\|small/double` | volume, press, volume, press inside the window → `ear-*-double` |
| `POST /control/volume?level=<n>&db=<n>` | grp 27, 12-byte payload |
| `POST /control/audio-id?id=<n>` | grp 20 fn 6803 (CC3200) / 16096 (ESP32) |
| `POST /control/report-audio-id` | same, using the audio id parsed from the last downloaded TAF |
| `POST /control/cycle?count=<n>&hold=<dur>&gap=<dur>` | **the load generator**: N place/lift cycles at a fixed cadence |
| `POST /control/stall?ms=<n>` | stop reading for N ms without disconnecting (omit `ms` to stall until `/control/resume`) |
| `POST /control/resume` | release a stall |
| `POST /control/disconnect` | drop the RTNL connection; the writer redials with backoff |
| `POST /control/telemetry` | firmware version, region, RTNL/production domains |
| `GET /control/report` | JSON: every assertion, every measurement, the active assumption set |
| `GET /api/sse` (mock's own) | mirror of the observed teddyCloud events, for eyeballing |
| `GET /` | HTML control page |

An in-flight content body is aborted by `--drop-after=<n>` rather than by an endpoint: the abort has
to happen at a known byte offset for the following `Range: bytes=N-` request to mean anything, and an
operator-timed request cannot hit that mark. `--range-offset=resume` turns the placement into
drop-then-resume.

`/control/cycle` is the feature the plan actually needs: 200 lift/place cycles at a fixed 3 s gap in
60 seconds, with exact timing and machine-checked invariants, replacing "5 cycles with an operator and
a stopwatch" (`plan_debug_sound.md` step 1.3).

## 10. Observation and assertions

### 10.1 What is observed

Two independent sources, deliberately:

1. **The SSE feed** from real teddyCloud (`GET /api/sse`) — timestamps and orders
   `TagValid`, `playback`, `ContentAudioId`, `ContentTitle`, `knock`, `pressed`, `charger`, `tilt`,
   `rtnl-raw-log2`/`-log3`, `keep-alive`. The raw-log frames matter because tag **removal has no SSE
   event at all** — only the raw log2 frame records it
   (`docs/teddycloud-static-analysis-sse.md:728-737`, `:1456-1478`).
2. **The mock's own download records** (§7.5) — what the box actually asked for and received.

### 10.2 Assertions

Grouped by what each one catches.

**Cycle invariants** — the Issue 1 acceptance criterion, automated:

| Assertion | Catches |
|---|---|
| every `/stream` attach is preceded by exactly one `play` for the expected URI | duplicate or missing plays |
| exactly one `playback=stopped` per lift, and zero during a place | the pause-vs-latch confusion (`ideal-shim-behavior.md` Part E step 4) |
| `ContentAudioId` is identical across all cycles for the same content | a re-render leaking through (§3.2) |
| `ContentAudioId` changes after the shim writes a new `.tap` | the re-link path |
| the tag UID in `TagValid` matches the requested UID | the mangled-UID class of bug |
| no `playback=started` is ever followed by `started` again without an intervening `stopped` | the missing-`break` double-emit (§3.1) treated as a state machine, not as noise |

**Timing** — the actual defect:

| Measurement | Meaning |
|---|---|
| `t_last_byte(cycle N)` → `t_first_byte(cycle N+1)` | the 10-20 s silence window. This is the number Issue 1 is about. |
| `t_play_sent` → `t_first_byte` | shim + Soloist + ffmpeg startup latency |
| `t_tag_valid` → `t_stream_attach` | teddyCloud's own ffmpeg startup (documented as ≥ 2 s, `:1538-1547`) |
| per-cycle byte total and bytes/s | whether the consumer kept up |

**Format** — carried over from spike §4, unchanged in substance:

chapter count equals the expected track count; `n_track_page_nums` matches and
`track_page_nums[]` is non-decreasing; chapter count ≤ 99 (`src/toniefile.c:389-392`); every chapter
decodes independently; the concatenation of chapters equals the whole TAF payload; chapter `NN`
matches source track `NN`; `tracks[NN]` in `tonies.json` matches chapter `NN`; display metadata
resolves.

**Protocol hygiene:**

the freshness body is under 4096 bytes; every RTNL frame is > 4 bytes, begins `00 00`, and has a
big-endian length below 65536; the CN is exactly 15 characters of the `b'[MAC]'` form; rUID offsets
are the teddyCloud ones; the UID byte order is the reverse of the integer.

Assertions are reported as a table with `PASS`/`FAIL` and the measured value, not as prose, so a CI
run or a loop of 200 cycles produces a diffable artefact.

---

## 11. Scenarios mapped to Part E

| Part E step | Mock support | Notes |
|---|---|---|
| 0 — measure Soloist resume semantics | **no** | needs a real Soloist; five minutes with a WS client |
| 1 — `/stream` lifecycle logging | **yes** | the primary target; this is what Issue 1 classification needs |
| 2 — Soloist WS state machine | no | belongs to `mock-soloist` |
| 3 — delete `Play("")`, real resume form | no | `mock-soloist` |
| 4 — consumer lifecycle as the liveness latch | **yes** | `--control/cycle` plus the `stopped`-per-lift assertion |
| 5 — command correlation, bounded queue | partly | `mock-soloist` for the producer side; the mock box supplies the *demand* side that makes correlation observable |
| 6 — SSE client-contract compliance | **no** | `cmd/mock-teddycloud` — a real teddyCloud in the path cannot emit malformed SSE |
| 7 — audio plane: silence gate | partly | `--speed 0` and `--stall-ms` create consumer starvation, which is the input condition |
| 8 — mock and test updates | **yes** | and this tool *is* part of step 8's deliverable |
| 9 — retire doc contradictions | no | documentation only |

### 11.1 The three load scenarios

1. **Baseline cycle.** N × (place, hold, lift) at a fixed gap. Measures the silence window directly.
   This is Issue 1.
2. **Consumer drop and resume.** `--drop-after` mid-body, then `Range: bytes=N-`. Distinguishes
   "consumer went away" from "consumer came back", and exercises teddyCloud's append path. This is
   the case where the shim should *not* see a new `/stream` request, and the assertion is that it
   does not.
3. **Starvation.** `--speed 0` for N ms, then resume. Drives the shim's buffer to empty without a
   disconnect, which is the input condition for the audio-plane work in step 7. This is the scenario
   that answers §7.4 item 5 of `ideal-shim-behavior.md` — *can teddyCloud's ffmpeg tolerate a starved
   `/stream`?* — which is the one **[E]** in that list a mock can actually settle.

---

## 12. What the mock cannot prove

Stated plainly, because the tool is only trustworthy if its limits are:

- **Whether real firmware *acts* on the bytes.** Whether the box skips between chapters, whether it
  tolerates the per-chapter Ogg framing, whether it cross-fades or clicks at boundaries, whether it
  shows per-chapter titles — all firmware behaviour. The spike §7 gate
  (`teddycloud --encode` a 3-chapter TAF, press next, observe) is unaffected by this tool and should
  still be run once, early.
- **Which firmware the real box runs.** `--firmware` is a guess. CC3200 and ESP32 use different
  function codes for the same logical event, which hints they behave differently. Settle it with a
  real box, or with `fake-server/server.py` (the spike's inverse tool) against a real box.
- **Whether a real box prefers `/v2/content` or `/v3/*`, or ever falls back.** Against itself this is
  only a choice. The mock makes both paths available so either can be chosen deliberately; it cannot
  report which one a real box uses.
- **Anything physical.** Speaker output, acoustics, button feel, gesture classification quality.
- **TeddyCloud's own correctness.** A mock cannot distinguish "the shim is wrong" from "teddyCloud is
  wrong". Every report therefore names teddyCloud's version and commit.

The general rule: **the mock verifies the shim given a specified box behaviour. It does not verify
the specified box behaviour.** Hence §8 — assumptions are flags, and the active set is part of every
report.

---

## 13. The Soloist gap

This mock covers the demand side. It says nothing about the producer.

`ideal-shim-behavior.md` Part E steps 2, 3, and 5 — Soloist state machine, real `play` resume form,
command correlation — need a controllable Soloist WebSocket server. Two options:

1. **Run real Soloist.** Requires Premium and a paired session; the protocol is not documented.
2. **`mock-soloist`.** The command set and event catalogue are fully documented in
   [`docs/research/spotify-soloist.md`](research/spotify-soloist.md:130-174), so this is cheap: a
   `gorilla/websocket` server (already a dependency) that emits scripted `playback_state`,
   `track_changed`, `position_sync`, `error` events and answers `get_state`.

`mock-soloist` should be a separate command in the same pass, and it must **parameterise the two
unresolved semantics from §7.4 items 1 and 2 of `ideal-shim-behavior.md`** (what `play` with no `uri`
does when nothing is loaded, and what `play` with the currently-loaded uri does to
`playback_state.position.position_ms`) rather than baking in one answer — the same principle as §8.

Together, `mock-toniebox` + `mock-soloist` + `cmd/mock-teddycloud` cover the whole of Part E except
step 0, which is a five-minute measurement against a real Soloist and should stay that way.

---

## 14. Implementation layout

```
internal/toniebox/
  rtnl.go        — protobuf field encoding + length-prefix framing (no protobuf dependency)
  rtnl_test.go   — golden byte tests against §6.7; required-field, <65536-length and
                   NUL-termination guards (§16.2)
  events.go      — protocol constants, firmware profiles, UID/rUID conversion, frame constructors
  freshness.go   — freshness-check request/response encode/decode
  freshness_test.go
  certs.go       — throwaway CA (CN "TeddyCloud") signing a leaf whose CN is the box MAC
  certs_test.go  — asserts the 12-char subject, the CA issuer CN, and the derived overlay id
  taf.go         — TAF header parse; audio id is field 3, varint
  taf_test.go
  box.go         — box state machine: uptime/sequence, tag frames, gestures, playback,
                   and the switchable assumptions of §8
  box_test.go    — ear-press sequences, tag framing, rUID round-trip, telemetry frames,
                   reported audio id per §8.2 policy
  client.go      — API/web clients, RTNL connection, auth decoration
  download.go    — freshness-check → content-meta → Range choice → paced read, with the
                   404-only fallback of §16.4 and the metadata-sourced audio id of §16.1
  download_test.go — a fake teddyCloud (freshness, content-meta, chapters, v2) covering the
                   Range lever, the fallback, the ceiling and the stall gate

cmd/mock-toniebox/
  main.go        — flags, wiring, banner, RTNL writer, cycle generator
  control.go     — the /control/* action table, with caller error separated from upstream
                   failure so a typo answers 400 rather than 502
  control_test.go — routing, parameter validation, status codes, SSE base-URL derivation,
                   cycle count parsing and the cancellation-aware sleep helper
  observe.go     — SSE observer and parser
  report.go      — cycle recorder, measurement table, assumption stamping
  report_test.go  — the §10.2 assertions, the SSE parser, and report stability
```

The protocol encoding lives in `internal/toniebox` rather than inside `cmd/` because it is genuinely
reusable and because the tests need it without the command's flag surface. This matches the repo's
`internal/` + `cmd/` split. Everything else follows `cmd/mock-teddycloud`'s conventions: `slog` with
a `mock-toniebox:` prefix, `flag` package, control page served from the same process.

Dependencies: **none added.** `crypto/tls`, `crypto/x509`, `encoding/binary`, `net/http` from the
standard library; the SSE feed is a hand-rolled `bufio.Scanner` over `text/event-stream`, in the
shape `internal/sselistener` already establishes.

## 15. Open questions

Carried from the spike, plus what this design surfaced.

1. ~~Does `/v3/content-meta` work for a `CT_SOURCE_STREAM` source?~~ **Answered — yes.** See §16.1.
2. Which firmware does the real box run — CC3200 or ESP32? Still open; needs hardware.
3. Does a real box request `/v2/content` or `/v3/*` first, and does it ever fall back? Still open;
   needs hardware. `--fallback` expresses both answers.
4. What do `field2`/`field6` in the freshness **response** actually drive? teddyCloud sends constants
   regardless (`toniebox.field2 == 7` and `field6 == 1` only when `overrideFields`, default `FALSE` —
   `src/handler.c:527-557`, `:660-672`).
5. Does `SERTY_BOTH` let the mock use the plain-HTTP listener and skip TLS during development?
6. Is the 64 KB RTNL length ceiling implied by `src/handler_rtnl.c:111` a latent bug or intentional?
   Worth reporting upstream independently of this project. See also §16.2 — the adjacent
   *string payload* handling in the same handler is a confirmed crash.
7. Does `Range: bytes=N-` against a live stream restart ffmpeg or continue the existing process?
   **Partly answered** — see §16.3. Still open for the stream-backed case that matters.

---

## 16. What running the mock changed

The design above was written from source. These are the things that only became visible once the
mock ran against a live teddyCloud v0.7.0 (`78c3b38`, AddressSanitizer build), which is the point of
§1 — a design read off the source is a hypothesis about the tool, not a result.

### 16.1 `/v3/content-meta` works for a stream source — but carries no audio id in the body

Answering open question 1: it works. A `CT_SOURCE_STREAM` content responds with a proper chapter
list and `version` set to the TAF audio id (measured: `431165001` for the sandbox figurine).

The part the design did not anticipate is what §7.3's TAF-header reasoning implies for the v3 route.
`/v3/chapter` serves chapter 0 from offset 4096 with `header_size=0`
(`src/handler_cloud.c:1535-1553`), so **no v3 response body contains a TAF header at all.** The audio
id is therefore only available as `content-meta`'s `version`, and a mock that parses it off the
downloaded bytes never learns it: it reports `0` forever, and the `ContentAudioId` correlation in
§10.2 cannot fire — an assertion that passes vacuously is worse than no assertion, because it looks
like coverage. The audio id now comes from the metadata on the v3 route and from the body on the v2
route, which is where each one actually lives.

### 16.2 teddyCloud crashes on a string-carrying RTNL frame with no NUL — confirmed, worth reporting

§6.3 warned that teddyCloud performs no length checking on `field6`. The sharper problem is on the
string side, and it is a crash rather than a misparse.

For the firmware and region/domain functions teddyCloud casts the payload straight to `const char *`
and passes it to `settings_set_string_id`, which ends in `strdup`
(`src/handler_rtnl.c:440-482`, `src/settings.c:1546`). protobuf-c allocates a `bytes` field exactly
its declared length and appends **no terminator**. So the NUL has to be inside `field6`: a payload of
`"105"` with no trailing zero makes teddyCloud `strdup` past the end of its own allocation. Against
the ASan build this is an immediate `heap-buffer-overflow` in `strdup` and the process aborts — one
malformed frame from one box takes the whole server down, taking every other box with it. Measured,
not inferred.

This was found because the mock's own telemetry frames had that bug and teddyCloud died mid-run; the
lesson recorded in the code is that the encoder's obligation is stronger than "the value is
non-empty", and `TestStringFramesAreNULTerminated` now asserts it on every string frame the mock can
emit.

### 16.3 `Range: bytes=N-` against an on-disk TAF restarts the chapter, it does not append

Open question 7, measured with `--range-offset resume --drop-after`: the first request gets `200`
and the follow-up `Range: bytes=16384-` gets `206`, but teddyCloud's own log shows
`Splitting TAF for chapter 0: start=4096, end=45056, header_size=0` **again** — it re-serves the
chapter from the start of its payload rather than continuing the partial TAF.

That is for an on-disk `CT_SOURCE_TONIE` source, which is the only content the sandbox has. It does
**not** settle the stream-backed case that the shim actually uses, because that path goes through
ffmpeg and is exactly where `src/handler_cloud.c:645` matters. So the question is narrowed rather
than answered: the mock can produce the request, and the remaining work is a content source that is a
stream. Worth re-testing when the shim is in the loop.

### 16.4 The fallback must key on 404, not on any error

§8.2 specified `--fallback on-404`. Implemented as "retry the other path on any error", it silently
destroyed the scenario it exists to produce: a deliberate `--drop-after` abort was caught, retried on
`/v2/content`, and came back as a **successful** v2 download — so the consumer-drop case reported a
pass. A 500 or an over-long chapter list was likewise masked by a v2 retry that succeeded for a
different reason. Fallback now triggers only on a 404, which is the one condition under which the
other route is worth trying.

### 16.5 Two measurements in §10.2 were unreachable as written

Both produced plausible-looking reports that could not detect the defect they name:

- **`ContentAudioId` is identical across cycles.** With no audio id ever learned (§16.1) the set of
  observed ids was empty, and "at most one distinct id" passed for the wrong reason.
- **`exactly one playback=stopped per lift`.** The final lift's stop is teddyCloud's *reaction* to a
  frame, published after the run's last cycle is already recorded, so a correct box missed by exactly
  one every run and the invariant always failed. The cycle generator now settles briefly after the
  last lift before reporting.

### 16.6 Verification

Run against `teddycloud-e2e` (`ghcr.io/toniebox-reverse-engineering/teddycloud:latest`, ASan build)
with `--teddycloud https://localhost:8443 --sse http://localhost:8080`:

| Check | Result |
|---|---|
| certificate gate | `Issuer: 'TeddyCloud'`, `Subject: '00aabbccdd00'` — §5.1's two-level chain accepted |
| telemetry frames | teddyCloud logs `txt=105`, `txt=2.105.00`, `txt=EU`, `txt=prod.teddyrock.com`; no ASan report, server stays up |
| 4-cycle baseline | `stopped_per_cycle 1.00`, `content_audio_ids [431165001]`, `distinct_ruids 1`, `failed_cycles 0` — all PASS |
| inter-cycle gap | ~5.6 s mean, ~5.7 s max against a 2 s `hold`+`gap` schedule — the tool's headline number, and it is the one Issue 1 turns on |
| drop + resume | `200` then `206` with `Range: bytes=16384-` (§16.3) |
| SSE translation | `playback` `starting`/`started`/`stopped`, `TagValid`, `VolumeLevel`, `VolumedB`, `rtnl-raw-log2`/`-log3` all observed |

The sandbox content is a single 40 KiB chapter served instantly, so the gap figure demonstrates the
measurement works rather than being a result about the 10-20 s silence. A meaningful number needs the
shim streaming a real TAF behind the content source.