# Spike: Mock Toniebox as a Placement-Driven Format Verifier

**What this is.** teddyCloud is **not** mocked — the real server runs. The Spotify→tonie shim is the
code under development. This mock is a verification harness on the **consumption side only**: you
tell it *figurine X is placed*, it drives the placement handshake against the real teddyCloud
exactly as a box would (freshness-check → content-meta → chapters), then inspects what comes back to
confirm **the tracks arrived in the correct format**.

**Scope.** Format of what teddyCloud serves. The Spotify capture side is the shim's own problem and
is deliberately not modelled here.

**Why it matters.** The shim's output can be wrong in a way that is invisible until something
consumes it — a chapter boundary that does not line up with a track boundary, an ordering that got
shuffled, a track that came back undecodable. This tool answers "is the format right?" in seconds,
against real teddyCloud, without a figurine on a box.

**What it cannot answer.** Whether the box firmware *acts* on correct bytes — see §7.

Claims are tagged **[P]** (proved by reading the code — `file:line` against
`/var/home/jan/ws/teddycloud` @ `78c3b38`, v0.7.0) or **[E]** (empirical / firmware behaviour).

---

## 1. Placement as the entry point

Everything the mock does hangs off one user-level action: *place figurine X*. That triggers the same
sequence a real box runs, which is the whole point — you never have to hand-craft HTTP requests.

```
place(uid X)
   │
   ├─ POST /v1/freshness-check      →  is X stale?
   │      TonieFreshnessCheckRequest{ tonie_infos:[{uid, audio_id}] }
   │      ← TonieFreshnessCheckResponse{ tonie_marked:[...] }
   │
   ├─ GET  /v3/content-meta/<ruid>  →  how many chapters, what are their sizes
   │      ← [ {name:"teddycloud_00_<ruid>", fileSize:N}, ... ]
   │
   ├─ GET  /v3/chapter/teddycloud_NN_<ruid>   →  the actual audio, NN = 0..count-1
   │
   └─ inspect: chapter count, decodability, order, boundaries, durations   ← THE POINT
```

The routes are registered at `src/server.c:103-182`. The relevant ones:

```c
{REQ_POST, "/v1/freshness-check", SERTY_BOTH, &handleCloudFreshnessCheck},   /* :173 */
{REQ_GET,  "/v2/content",         SERTY_BOTH, &handleCloudContentV2},       /* :172 */
{REQ_GET,  "/v3/content-meta",    SERTY_BOTH, &handleCloudContentMetaV3},   /* :182 */
{REQ_GET,  "/v3/chapter",         SERTY_BOTH, &handleCloudChapterV3},      /* :181 */
{REQ_ANY,  "*binary",             SERTY_BOTH, &handleRtnl},                /* :105 */
```

The upstream comments group these explicitly: TB1 is `/v1/*` + `/v2/content` (`:167`), TB2 is
`/v3/*` (`:176`). Implement **both** content paths — you do not yet know which firmware your real
box runs, and finding out is itself an **[E]** this mock can help settle.

Message schemas, all in `proto/`, all proto2, all small enough to hand-roll without `protoc`:

| Schema | Message | Fields |
|---|---|---|
| `toniebox.pb.freshness-check.fc-request.proto:3` | `TonieFreshnessCheckRequest` | `repeated TonieFCInfo tonie_infos = 1` |
| `toniebox.pb.freshness-check.fc-request.proto:7` | `TonieFCInfo` | `fixed64 uid = 1`, `fixed32 audio_id = 2` |
| `toniebox.pb.freshness-check.fc-response.proto:3` | `TonieFreshnessCheckResponse` | `repeated fixed64 tonie_marked = 1`, plus `max_vol_spk`, `slap_en`, `slap_dir`, `led` |
| `toniebox.pb.rtnl.proto:3` | `TonieRtnlRPC` | `TonieRtnlLog2 log2 = 2`, `TonieRtnlLog3 log3 = 3` |

A freshness-check request for one tonie is about 15 bytes on the wire. Nothing hidden. **[P]**

### 1.1 RTNL framing — the one gotcha

RTNL is the box's telemetry channel, at path `*binary` (no leading slash in the table,
`src/server.c:105`). The body is a stream of length-prefixed protobuf
(`src/handler_rtnl.c:85-145`):

```
while (bytes remain):
    protoLength = (buf[pos]<<24)|(buf[pos+1]<<16)|(buf[pos+2]<<8)|buf[pos+3]   /* :102 */
    if protoLength == 0 or buf[pos] != 0 or buf[pos+1] != 0:                   /* :110-114 */
        return ERROR_FAILURE                                                  /* :114 */
    rpc = TonieRtnlRPC.unpack(buf[pos+4 : pos+4+protoLength])                  /* :131 */
```

`:102` reads a full 32-bit big-endian length, but `:110-114` also rejects anything whose top two
bytes are non-zero — an undocumented 64 KB ceiling. So **emit the length with its high two bytes
zero**. A little-endian prefix, or a Python `>I` struct packing a length over 65535, gets the whole
request rejected with no useful diagnostic. **[P]**

Trailing bytes after the last complete packet are preserved for the next call
(`src/handler_rtnl.c:145`), so multiple RTNL messages may be batched into one POST. **[P]**

### 1.2 Synthesising the placement event

You can make teddyCloud believe a figurine was placed, purely over RTNL — useful for asserting the
SSE/MQTT event sequence the shim or UI depends on.

`TonieRtnlLog2` selects an event by a `(function_group, function)` pair
(`src/handler_rtnl.c:336-490`). Values are in `include/handler_rtnl.h`:

| Group | Value | Function | Value | Effect |
|---|---|---|---|---|
| `RTNL2_FUGR_TAG` | 15 | `RTNL2_FUNC_TAG_VALID_CC3200` | 8627 | `tbs_tag_placed(uid, true)` |
| | | `RTNL2_FUNC_TAG_VALID_ESP32` | 16065 | `tbs_tag_placed(uid, true)` |
| | | `RTNL2_FUNC_TAG_INVALID_CC3200` | 8646 | `tbs_tag_placed(uid, false)` |
| | | `RTNL2_FUNC_TAG_INVALID_ESP32` | 15452 | `tbs_tag_placed(uid, false)` |
| | | `RTNL2_FUNC_TAG_VALID_REMOVED_CC3200` | 8630 | `tbs_tag_removed(uid, true)` |
| | | `RTNL2_FUNC_TAG_VALID_REMOVED_ESP32` | 16067 | `tbs_tag_removed(uid, true)` |

`TonieRtnlLog3` uses a single `type` discriminator instead (`include/handler_rtnl.h:89-99`),
dispatched at `src/handler_rtnl.c:267-324`:

| `RTNL3_TYPE_*` | Value | Effect |
|---|---|---|
| `PLAYBACK_STARTING` | 11 | `tbs_playback(TBS_PLAYBACK_STARTING)` |
| `PLAYBACK_STARTED` | 12 | `tbs_playback(TBS_PLAYBACK_STARTED)` |
| `PLAYBACK_STOPPED` | 13 | `tbs_playback(TBS_PLAYBACK_STOPPED)` |

Refs: `include/handler_rtnl.h:18-26` (groups), `:66-71` (tag functions), `:89-99` (log3 types);
dispatch at `src/handler_rtnl.c:336-348` (tag), `:317-324` (playback).

Send `function_group=15, function=8627` with the 8-byte big-endian uid in `field6`, and teddyCloud
registers a placement. `field6` is read via `read_big_endian64()` (`src/handler_rtnl.c:340`), which
composes two `read_big_endian32` calls (`:165-168`) — no sign-extension issue on this path. **[P]**

---

## 2. The freshness-check audio_id trap

Getting this wrong makes teddyCloud believe content is permanently stale, which looks exactly like a
server bug and would otherwise cost a hardware debugging session.

teddyCloud stores custom tonie audio_ids **reduced** by a constant:

```c
#define TEDDY_BENCH_AUDIO_ID_DEDUCT 0x50000000                                   /* include/toniefile.h:33 */
tap_param.tap->audio_id = time(NULL) - TEDDY_BENCH_AUDIO_ID_DEDUCT;            /* src/handler_cloud.c:699 */
```

A `.tap` written today stores roughly `0x68…` and is classified "custom" precisely *because* it is
below the threshold (`src/handler_cloud.c:904`). Both sides are normalised up before comparison
(`src/handler_cloud.c:963-977`):

```c
checkAudioIdForCustom(&custom_box, date_buffer_box, boxAudioId);   /* :963 */
if (custom_box)
    boxAudioId += TEDDY_BENCH_AUDIO_ID_DEDUCT;                     /* :966 */

serverAudioId = tonieInfo->tafHeader->audio_id;                    /* :971 */
if (custom_server)
    serverAudioId += TEDDY_BENCH_AUDIO_ID_DEDUCT;                   /* :975 */

tonieInfo->updated = boxAudioId < serverAudioId;                   /* :977 */
```

The direction is counter-intuitive: **`updated` is true when the box's id is *older* than the
server's.** The box reports what it holds cached; if the server is newer, the box is stale. **[P]**

So the mock must model a cached audio_id:

- **Store the audio_id exactly as it appeared in the TAF header you downloaded** — that is what real
  hardware does. Echoing it back reproduces "already current" (`updated = false`).
- **Send `0`** to reproduce a box with nothing cached (`updated = true`).
- `settings->cloud.updateOnLowerAudioId` inverts the comparison (`:978`) and
  `settings->cloud.prioCustomContent` adds custom/non-custom overrides (`:979-985`). Pin `config.ini`
  in tests so these cannot silently change an expected outcome. **[P]**

---

## 3. TLS and ports

The box listener is HTTPS. `httpServerSelectBoxCertificate()` picks the certificate by **SNI** — no
server name means the TB1 cert chain, SNI present means TB2 (`src/server.c:709-720`, `:682-692`,
`:717-720`). One TLS-handshake change switches which path you exercise.

Authentication is a no-op. `httpServerAuthCallback()` returns `HTTP_ACCESS_ALLOWED`
unconditionally (`src/server.c:661-663`) and `httpAddAuthenticateField()` returns 0 (`:665-668`).
The box's `?auth=` token and `Authorization: BD …` header are parsed and passed around but never
verified. The mock may send anything, including nothing. **[P]**

Ports come from `core.server.http_port`, `core.server.https_web_port`,
`core.server.https_api_port` (`src/server.c:918-933`). The box routes are `SERTY_BOTH`, so a plain
HTTP listener may be usable during development — worth confirming against your `config.ini` rather
than assuming; which listener serves `SERTY_BOTH` is a boundary I have not traced. **[P/E]**

---

## 4. Format verification — the point of the tool

Given a placement, assert the track format is right. Grouped by what they catch.

### 4.1 Structure

- **Chapter count equals the expected track count.** From `/v3/content-meta/<ruid>` the item count
  must equal the number of tracks the shim linked. Names are `teddycloud_%02d_<ruid>` — the format
  string is at `src/handler_cloud.c:1446` (`%02d`), so chapter numbers are zero-padded to exactly
  two digits.
- **TAF header self-consistency.** `n_track_page_nums` matches the chapter count, and
  `track_page_nums[]` is monotonically non-decreasing. Schema at
  `proto/toniebox.pb.taf-header.proto:3-8`; the page number is recorded at
  `src/toniefile.c:393` (`track_page_nums[n_track_page_nums++] = taf_block_num`), and the count is
  bounded immediately above at `:389`.
- **Chapter count is ≤ 99.** `toniefile_new_chapter` refuses at
  `n_track_page_nums >= TONIEFILE_MAX_CHAPTERS - 1` (`src/toniefile.c:389-392`), and
  `tap_generate_taf` has a matching `char source[99][PATH_LEN]`
  (`src/tonie_audio_playlist.c:189`). Catches a playlist linked in excess of
  the cap — silently truncated otherwise.

### 4.2 Audio validity

- **Every chapter decodes independently.** `ffmpeg -i chapter_NN.ogg` should yield 48 kHz stereo
  Opus for each. A chapter that only decodes when concatenated with its neighbour means a bad
  boundary.
- **Concatenated chapters equal the whole TAF payload.** Decode the TAF payload and the
  concatenation of all chapter bodies; the audio must match. This is the single strongest format
  assertion available and should be the one you trust most.
- **Chapter framing asymmetry.** A 512-byte constant is introduced only when `n_chapters > 1`, and
  `total_audio_bytes` switches to the TAF header's `num_bytes` (`src/handler_cloud.c:1434-1438`).
  Chapter 0 additionally goes through `skip_taf_header` (`:1551`). Determine the exact per-chapter
  layout from your first real capture and pin it as a fixture — do not guess it from the source.
- **Format reference:** teddyCloud's encoder feeds `ffmpeg_stream` at 48 kHz stereo
  (`src/toniefile.c:595`). The standalone WASM test binary documents its raw input as 48 kHz
  **mono** (`wasm/taf_encoder_test.c:20-21`) — a different path. Use the `--encode` /
  `toniefile.c` path as the reference, since that is the one the shim goes through. **[P]**

### 4.3 Ordering and boundaries

- **Chapter NN matches track NN.** Compare a fingerprint of decoded chapter `NN` against source
  track `NN` from the album metadata — an energy envelope or a checksum of the first N decoded
  samples is enough. This catches order shuffles, which are otherwise invisible until someone
  listens.
- **No boundaries lost or duplicated.** Total concatenated duration should equal the sum of track
  durations within tolerance (say ±2 s per track), not merely the album length.
- **Per-chapter duration.** Each chapter's decoded duration should be within a small tolerance of
  its track's metadata duration. A chapter noticeably long or short means the boundary landed in
  the wrong place.

### 4.4 Metadata

- **`tracks[NN]` in `tonies.json` matches chapter `NN`.** A mismatch shows a wrong title on a skip.
  This is the easiest thing to get wrong when order is decided at render time, and it is invisible
  to every other check here.
- **Display metadata resolves.** teddyCloud reads `title` and `pic` for the box display
  (`src/toniesJson.c:356`, `:386`). A tonie that plays but shows nothing is not a working result.

### 4.5 Lifecycle

- **Re-linking is observed as a format change.** After the shim writes a new `.tap` with a new
  `audio_id`, the tonie must go stale and re-render — the check at
  `src/tonie_audio_playlist.c:186` compares the `.tap` audio_id against the existing TAF. Place
  again and assert the new tracklist replaced the old one, with the right count and order.
- **Unknown chapter id returns 404** (`src/handler_cloud.c:793-802`).
- **v2 fallback works.** `GET /v2/content/<ruid>` with `Range` returns the whole TAF, for pre-v3
  firmware.

---

## 5. Give it a state machine, not a request script

Real boxes are stateful: they hold a cached audio_id, they cache downloaded content, they re-check
on a timer. A mock that replays a fixed request sequence will pass while every state transition rots
— and the transitions are where format bugs live.

The loop you will actually run:

```
for each link change:
    shim writes .tap / content.json / tonies.json for uid X
    place(X)
        if cached_audio_id == stored_audio_id:   assert updated == False   # no needless re-fetch
        else:                                    assert updated == True
        fetch content-meta                       assert chapter count == expected
        fetch chapters 0..N-1                    §4.1-4.4
        store audio_id from the TAF header we downloaded
    place(X) again
        assert no re-fetch happened, still plays
```

The second `place(X)` matters: it distinguishes "teddyCloud correctly serves a cached tonie" from
"teddyCloud re-renders on every placement". Those are different bugs and only the second placement
tells them apart.

---

## 6. Cost

One Python script, two generated protobuf modules (or hand-rolled — the messages are tiny), and an
assertion suite. Roughly 200-300 lines. `requests` and `protobuf` are the only dependencies, and
`tests/requests.py` shows the house style for driving the HTTP surface.

---

## 7. What it cannot answer, and the one-time gate

This tool proves teddyCloud serves the right bytes. It says nothing about whether box firmware
*acts* on them:

- **Whether the box skips between chapters.** The entire premise of the design — the reason the
  shim renders a multi-chapter TAF rather than a stream. **[E]**
- Whether the box tolerates the per-chapter Ogg header framing. **[E]**
- 48 kHz stereo → the box's actual output path, and the speaker. **[E]**
- Cross-fade versus click at chapter boundaries; whether a short fade is needed in the render. **[E]**
- Whether the box shows per-chapter `tracks[]` titles or only the album title. **[E]**
- Placement-triggered re-fetch versus timer-driven, and CC3200 versus ESP32 differences — the code
  has *separate* function codes per chip for the same logical event
  (`src/handler_rtnl.c:338-348`), which hints they behave differently. **[E]**
- The licensing constraint noted in `shim-design.md` §8.

One of these is a go/no-go, so run it once, early, and outside the loop:

> `teddycloud --encode` three short tracks into one TAF (`src/main.c:676`) → one figurine on the box
> → press next. Does it skip?

Ten minutes, no Spotify, no shim, no capture wait; teddyCloud alone serves it. If the box will not
skip on a 3-chapter TAF that teddyCloud generated by itself, the static-album premise fails and the
right next move is a different architecture — not an hour of capture followed by a puzzled debugging
session.

`teddycloud --encode-test out.taf` is a related built-in: it writes a 99-chapter sine-wave TAF
(`src/main.c:711`, generator at `src/main.c:610-624`), attempting 100 chapters against the hard
limit of 99. Use it as a fixture for the chapter-count assertion. The exact intent is inferred; the
numbers are certain. **[P]**

`fake-server/server.py` is the inverse of this tool — a TLS MITM proxy that impersonates the cloud
and logs a real box (`fake-server/server.py:40-48`). Both directions are worth having.

---

## 8. Open questions

1. Which firmware does your box run — CC3200 or ESP32? It selects the tag RTNL function codes and
   the OTA folder (`src/handler_cloud.c:1596-1600`), and you need to know before the mock emits
   correct placement events.
2. Does a real box request `/v2/content` or `/v3/content-meta` first, and does it ever fall back?
   The mock can answer this only against a real box; against itself it is just a choice.
3. What do `field2` / `field6` in the freshness-check *response* actually drive? The schema marks
   them required (`proto/toniebox.pb.freshness-check.fc-response.proto:5-11`) but teddyCloud may
   send constants regardless.
4. Does `SERTY_BOTH` let the mock use the plain-HTTP listener and skip TLS entirely in dev? (§3)
5. Is the 64 KB ceiling implied by `src/handler_rtnl.c:110-114` a latent bug or intentional? If
   latent, it is worth reporting upstream independently of this project.