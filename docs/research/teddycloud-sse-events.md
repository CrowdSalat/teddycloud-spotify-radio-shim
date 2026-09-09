# Research: Real Teddycloud SSE events (Phase 6 discovery)

Captured 2026-09-09 (10:46–10:56 CEST) against the real Teddycloud in namespace `app-teddycloud`.

- Access: `oc port-forward svc/teddycloud 8080:80 -n app-teddycloud`, then `curl -sN http://localhost:8080/api/sse`. Port 80 hits the teddycloud container directly — the oauth-proxy sidecar (4180) is bypassed, no login needed.
- Raw capture: `docs/research/teddycloud-sse-capture.txt` (includes `rtnl-raw-log*` transport noise — ignore it).
- Tonie UID used: `E00403500EEA4BF2`.

The file is the **byte-for-byte reference** for fixing `cmd/mock-teddycloud` in Phase 6.

---

## Wire format

SSE framing is standard: an `event:` line, a `data:` line, terminated by a blank line. Every event's `data` is a JSON object `{ "type":"<event>", "data":"<value>" }` where `<value>` is always a **string**.

```
event: TagValid
data: { "type":"TagValid", "data":"E00403500EEA4BF2" }

```

No `id:`, no `retry:`. Heartbeat is a keep-alive every ~16 s.

---

## Event inventory

| Event | `data` value | When |
|---|---|---|
| `keep-alive` | `""` | every ~16 s |
| `TagValid` | tonie NFC UID hex, e.g. `E00403500EEA4BF2` | figurine placed (valid tag) |
| `playback` | `starting` / `started` / `stopped` | playback state transitions |
| `ContentAudioId` | `436906887` | current audio-stream id (after TagValid) |
| `ContentTitle` | `Unknown` | current title (no metadata here) |
| `VolumeLevel` | `9` / `10` / `11` / `12` | new volume after an ear press |
| `VolumedB` | `-3` / `-6` / `-9` / `-12` | new gain in dB after an ear press |
| `pressed` | `ear-big` / `ear-small` / `ear-small-double` | box ear press |
| `knock` | `forward` / `backward` | box tilt |

---

## Gesture → SSE sequence (observed)

### Place a valid figurine

```
event: TagValid
data: { "type":"TagValid", "data":"E00403500EEA4BF2" }

event: playback
data: { "type":"playback", "data":"starting" }

event: playback
data: { "type":"playback", "data":"started" }    # "started" repeats; Content* follows

event: ContentAudioId
data: { "type":"ContentAudioId", "data":"436906887" }

event: ContentTitle
data: { "type":"ContentTitle", "data":"Unknown" }
```

### Lift the figurine

```
event: playback
data: { "type":"playback", "data":"stopped" }
```

**No `TagInvalid` event exists.** The real server never emits it — a lift surfaces only as `playback: stopped`.

### Right ear (volume-up ear)

```
event: pressed
data: { "type":"pressed", "data":"ear-big" }

event: VolumeLevel
data: { "type":"VolumeLevel", "data":"12" }

event: VolumedB
data: { "type":"VolumedB", "data":"-3" }
```

### Left ear (volume-down ear)

```
event: pressed
data: { "type":"pressed", "data":"ear-small" }

event: VolumeLevel
data: { "type":"VolumeLevel", "data":"10" }

event: VolumedB
data: { "type":"VolumedB", "data":"-9" }
```

Left-ear double press appears as `pressed` / `ear-small-double`. Knock/tilt is `knock` / `forward` or `backward`.

---

## Findings vs current assumptions

The discovery **rejects two assumptions** of `docs/STRUCTURE.md` Phase 5/6 and the `internal/sselistener` doc comment:

1. **There is no `TagInvalid`.** The mock's `figurine-lifted` and the listener's `figurine-lifted`/`TagInvalid` → Pause branch will never fire against the real server. Lifting the figurine must be detected from `playback` + `stopped` → send `pause`.
2. **Ear event names are wrong.** The mock emits `right-ear-slap`/`left-ear-slap`; the real server emits `pressed` + `ear-big`/`ear-small`. The mock must emit the real names and shape byte-for-byte.

Other deltas:

- `TagValid` carries the **tonie UID hex**, not a URI and not JSON. `extractURI` returns `""` → `Play("")`. This is already the documented "real events carry no URI" case — the URI arrives via the `/stream?spotify_uri=` request, not SSE.
- Ear presses are **accompanied by `VolumeLevel`/`VolumedB`** pairs. They are noise for the shim's control path (box-local volume) but should be reproduced in the mock for fidelity.
- Keep-alive interval is ~16 s (mock: 15 s) — align to 16 s.
- Knock events (`forward`/`backward`) follow tilts; they are irrelevant to the control path.

## Control mapping (shim)

| Physical action | SSE signal | Shim command |
|---|---|---|
| Figurine placed | `TagValid` | `play` (URI from `/stream` request) |
| Figurine lifted | `playback` `stopped` | `pause` |
| Right ear | `pressed` `ear-big` | `skip_next` |
| Left ear | `pressed` `ear-small` | `skip_prev` |
| Left-ear double | `pressed` `ear-small-double` | — (currently ignored) |