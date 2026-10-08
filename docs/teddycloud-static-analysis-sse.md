# teddyCloud SSE — Static Analysis Reference

**Purpose:** build an external application that subscribes to the teddyCloud SSE stream and uses
those events to control playback of a radio stream played through teddyCloud / the Toniebox.

**Repository:** `/var/home/jan/ws/teddycloud` · **Version:** v0.7.0 · **HEAD:** `78c3b38` (`v0.7.0`)

**Method:** static analysis of the source tree only. Claims are tagged:

- **[P]** *proven* — the code determines it
- **[E]** *empirical* — requires a real box to confirm; the code cannot answer it

All references are `file:line` on the checked-out tree.

---

## Table of contents

1. [Executive summary](#1-executive-summary)
2. [Transport: endpoint, routing, auth](#2-transport-endpoint-routing-auth)
3. [Wire format](#3-wire-format)
4. [Connection lifecycle, limits, concurrency](#4-connection-lifecycle-limits-concurrency)
5. [Complete event registry](#5-complete-event-registry)
6. [Event groups in detail](#6-event-groups-in-detail)
   - 6.1 [Transport / heartbeat](#61-transport--heartbeat)
   - 6.2 [Tag & playback lifecycle](#62-tag--playback-lifecycle)
   - 6.3 [Physical interaction & box status](#63-physical-interaction--box-status)
   - 6.4 [Command acknowledgements (`cmd`)](#64-command-acknowledgements-cmd)
   - 6.5 [Raw RTNL feed](#65-raw-rtnl-feed)
7. [RTNL function-code tables](#7-rtnl-function-code-tables)
8. [Radio / stream tonies on the SSE stream](#8-radio--stream-tonies-on-the-sse-stream)
9. [Control channel — how to actually drive the box](#9-control-channel--how-to-actually-drive-the-box)
10. [Client contract and reference implementation](#10-client-contract-and-reference-implementation)
11. [Known bugs, hazards, and doc drift](#11-known-bugs-hazards-and-doc-drift)
12. [Source index](#12-source-index)

---

## 1. Executive summary

### The one thing that changes the design

**SSE is strictly one-way (server → client).** It cannot control playback. The `cmd` events look like
commands but are **acknowledgements** emitted when a command arrived over **MQTT**. A working radio
controller needs three channels:

| Channel | Direction | Purpose |
|---|---|---|
| `GET /api/sse` | teddyCloud → app | Observe box state and user intent |
| MQTT `{topic}/box/{cn}/Cmd*/set` | app → teddyCloud | Stop the stream, set volume limits / LED / slap |
| `POST /content/json/set/<ruid>` | app → teddyCloud | Point a tag at a radio URL |

There is **no** server→box command channel at all. `MQTT_CONTROL.md:9-20` states it plainly:

> The RTNL protocol is a **one-way** notification channel (box → server). There is **no real-time
> command channel** from server to box. Settings are pushed to the box only via the freshness check
> response.

### Capability matrix

| Capability | Available? | Mechanism |
|---|---|---|
| Observe box state / user gestures | **Yes** | SSE `GET /api/sse` |
| Stop a running radio stream | **Yes** | MQTT `…/CmdStop/set` (server-side kill of ffmpeg) |
| Speaker / headphone volume **limit** | **Yes** | MQTT `CmdVolLimitSpk` / `CmdVolLimitHdp`, `0–3` |
| LED mode, slap-to-skip, slap direction | **Yes** | MQTT `CmdLed` (`0–2`), `CmdSlapEnabled`, `CmdSlapDirection` |
| Set the **actual** volume (0–16) | **No** | No protocol field; read-only |
| Pause / resume / mute / skip track | **No** | No server→box channel |
| Serve an external radio URL to a box | **Yes** | `content.json` → `source: "http(s)://…"` → ffmpeg → TAF |
| `GET /api/box/cmd` REST commands | **No** | Documented but **removed** in `fbd3fe5` |

### Event count

**18 distinct `event:` names.** No `id:`, no `retry:`, no `Last-Event-ID`, no replay, no per-box or
per-event filtering, at most **8** concurrent subscribers process-wide.

### Radio mode, in one paragraph

A radio tonie produces **the same SSE events as a cached TAF** — teddyCloud has no idea which is
which at the point where events are generated (`src/handler_cloud.c` emits *zero* SSE events).
The differences are only in the *values*: `ContentAudioId` becomes `time(NULL) - 0x50000000`
(a value **below** `0x50000000`, which doubles as a reliable radio fingerprint), `ContentTitle`
resolves via `tonie_model` or falls back to `"Unknown"`, and `ContentPicture` is only emitted when
the title lookup succeeded. Lifting the tag **does** stop the stream — via two independent
mechanisms. Details in [§8](#8-radio--stream-tonies-on-the-sse-stream).

---

## 2. Transport: endpoint, routing, auth

### 2.1 Route

```c
// src/server.c:165
{REQ_GET, "/api/sse", SERTY_WEB, &handleApiSse},
```

- **Method:** `GET` only. `POST /api/sse` → static-file fallback → **404**
  (`src/server.c:530-533` → `src/server_helpers.c:800-815`).
- **Ports:** `SERTY_WEB` means *web listeners only*
  (`src/server.c:527`):

  | Listener | Setting | Default | `/api/sse`? |
  |---|---|---|---|
  | HTTP | `core.server.http_port` | 80 | **yes** |
  | HTTPS web | `core.server.https_web_port` | 8443 | **yes** |
  | HTTPS box API | `core.server.https_api_port` | 443 | **no** → 404 |

- **Prefix quirk:** `osStrncmp(path, uri, pathLen)` with `pathLen == 8` (`src/server.c:524-525`), so
  `/api/sse`, `/api/sse/` and even `/api/sseFOO` all route here.
- **`queryString` is ignored** (`src/handler_sse.c:9`). No event filtering, no box/overlay
  selection, no `since=`/`id=`, no `Last-Event-ID`. Every subscriber gets every event.

### 2.2 Authentication — effectively none

| Gate | Location | Effect on `/api/sse` |
|---|---|---|
| `httpServerAuthCallback` | `src/server.c:661-663` | **Always returns `HTTP_ACCESS_ALLOWED`** |
| `Authorization: BD <hex>` / `Bearer <hex>` | `src/server.c:596-659` | Parsed, **never validated** (`// TODO: check JWT TOKEN` at `:635`) |
| Cookies | `http_server.h:118-121` | **Disabled** |
| Client-certificate auth | `src/server.c:516-520` | Requires `api_access_only` ⇒ never applies to `SERTY_WEB` |
| `security_mit` lock | `src/server.c:509-514` | Returns **200 + `text/plain`**, not 403 |
| `security_mit` UA blacklist | `src/handler_security_mit.c:133-140` | A User-Agent containing `Googlebot`, `Nmap`, `Burp Suite`, `Nuclei`, … latches an incident and **locks the frontend until restart** if `security_mit.lockAccess` (default `true`, `src/settings.c:152`) |

Also: `POST /api/auth/login` accepts any password — the check is commented out
(`src/handler_api.c:4605`).

**Implications.** teddyCloud ships no auth on the web port; users put a reverse proxy in front.
A browser `EventSource` **cannot** send an `Authorization` header and defaults to
`withCredentials=false` → cross-origin SSE carries no credentials; only same-origin cookies work.
**Do not use a scanner-like User-Agent.**

### 2.3 Response headers

`src/handler_sse.c:48-53`:

```c
    httpInitResponseHeader(connection);
    connection->response.contentType = "text/event-stream";
    connection->response.keepAlive = TRUE;
    connection->response.chunkedEncoding = TRUE;
```

Rendered by `httpFormatResponseHeader` (`src/cyclone/cyclone_tcp/http/http_server_misc.c:786-961`):

```
HTTP/1.1 200 OK
Connection: keep-alive
Keep-Alive: timeout=300, max=4294967294
Accept-Ranges: bytes
Access-Control-Allow-Origin: <core.allowOrigin>      <-- only if configured and non-empty (:918)
Content-Type: text/event-stream
Transfer-Encoding: chunked

```

- **No `Content-Length`** — the chunked branch wins (`http_server_misc.c:946-947`); the
  `Content-Length` write at `:950-953` is in the `else if (keepAlive)` branch and is not taken.
- **No `Cache-Control`** (`noCache == FALSE`, `maxAge == 0`, `http_server_misc.c:758`) ⇒
  intermediaries may buffer. Behind nginx/Cloudflare set `proxy_buffering off`.
- **HTTP/1.0 caveat:** `httpFormatResponseHeader` (`:805-815`) force-disables chunking *and*
  keep-alive for HTTP/1.0 ⇒ the stream becomes close-delimited.

### 2.4 Slot allocation and 503

```c
// src/handler_sse.c:9-36
error_t handleApiSse(HttpConnection *connection, const char_t *uri, const char *queryString, client_ctx_t *ctx)
{
    mutex_lock(MUTEX_SSE_CTX);
    SseSubscriptionContext *sseCtx = NULL;
    for (uint8_t channel = 0; channel < SSE_MAX_CHANNELS; channel++)
    { // Find first free slot
        sseCtx = &sseSubs[channel];
        if (!sseCtx->active) { break; }
        else { sseCtx = NULL; }
    }
    if (sseCtx == NULL)
    {
        mutex_unlock(MUTEX_SSE_CTX);
        TRACE_ERROR("All slots full, in total %" PRIu8 " clients\r\n", sseSubscriptionCount);
        httpInitResponseHeader(connection);
        connection->response.contentLength = 0;
        connection->response.statusCode = 503; // Service Unavailable
        connection->response.keepAlive = FALSE;
        connection->response.chunkedEncoding = FALSE;
        return httpWriteHeader(connection);
    }
```

The slot table is **one process-global array** (`src/handler_sse.c:6-7`) shared by both web
listeners. The 9th subscriber gets `503` with an empty body and a closed socket.

**Per the WHATWG SSE spec a non-200 makes `EventSource` fail permanently** (`readyState === CLOSED`,
no auto-retry) — you must implement your own backoff.

### 2.5 Status codes

| Situation | Code | Where |
|---|---|---|
| Success | `200` + chunked stream | `handler_sse.c:48-53` |
| All 8 slots used | `503` | `handler_sse.c:26-36` |
| Wrong method / wrong port | `404` | `server.c:530-533` |
| `security_mit` lock | `200 text/plain` | `server.c:510-514` |
| Header write fails | nothing sent, socket closed | `handler_sse.c:54-58`; error callback commented out at `server.c:534-537` |

---

## 3. Wire format

### 3.1 The write primitives

```c
112: error_t sse_startEventRaw(const char *eventname)
113: {
114:     mutex_lock(MUTEX_SSE_EVENT);
115:
116:     error_t error = NO_ERROR;
117:
118:     error = sse_rawData("event: ");
...
122:     error = sse_rawData(eventname);
...
126:     error = sse_rawData("\ndata: { \"type\":\"");
...
130:     error = sse_rawData(eventname);
...
134:     error = sse_rawData("\", \"data\":");
135:     return error;
136: }
```

```c
138: error_t sse_rawData(const char *content)
139: {
140:     error_t error = NO_ERROR;
141:
142:     mutex_lock(MUTEX_SSE_CTX);
143:     for (uint8_t channel = 0; channel < SSE_MAX_CHANNELS; channel++)
144:     {
145:         SseSubscriptionContext *sseCtx = &sseSubs[channel];
146:         if (!sseCtx->active) { continue; }
150:         sseCtx->error = httpWriteString(sseCtx->connection, content);
151:         if (sseCtx->error == NO_ERROR) { sseCtx->lastConnection = time(NULL); }
155:     }
156:     mutex_unlock(MUTEX_SSE_CTX);
157:
158:     return error;
159: }
```

```c
161: error_t sse_endEventRaw(void)
162: {
163:     error_t error = NO_ERROR;
164:     error = sse_rawData(" }\n\n");
165:     mutex_unlock(MUTEX_SSE_EVENT);
166:     return error;
167: }
```

```c
169: error_t sse_sendEvent(const char *eventname, const char *content, bool escapeData)
...
179:         if (escapeData) { error = sse_rawData("\""); ... }   // opening quote
188:         error = sse_rawData(content);                          // raw, NO escaping
194:         if (escapeData) { error = sse_rawData("\""); ... }   // closing quote
```

### 3.2 The exact frame

`sse_sendEvent(name, content, /*escapeData=*/true)` emits **9 fragments**:

| # | literal fragment | bytes |
|---|---|---|
| 1 | `event: ` | 7 |
| 2 | `<eventname>` | n |
| 3 | `\ndata: { "type":"` | 17 |
| 4 | `<eventname>` | n |
| 5 | `", "data":` | 10 |
| 6 | `"` | 1 |
| 7 | `<content>` | n |
| 8 | `"` | 1 |
| 9 | ` }\n\n` | 4 |

De-chunked (LF only, never CRLF):

```
event: TagValid
data: { "type":"TagValid", "data":"1122334455667788" }

```

Other verified examples:

```
event: playback
data: { "type":"playback", "data":"started" }

event: knock
data: { "type":"knock", "data":"forward" }

event: charger
data: { "type":"charger", "data":"on" }

```

**Each fragment is its own HTTP chunk** — `httpWriteString` (`src/handler.c:1037-1040`) →
`httpWriteStream` (`src/cyclone/cyclone_tcp/http/http_server.c:838-893`) wraps each call in a
complete chunk. A `TagValid` frame is 9 chunks (5 envelope + quote + content + quote + terminator);
a `rtnl-raw-log2` frame is 9–11 (11 when both `field6` and `field9` are non-empty, 10 with one,
9 with neither). Legal HTTP/1.1, transparent to standard clients, but a hand-rolled client must
de-chunk and dispatch on the blank line.

### 3.3 The `escapeData` parameter — critical

**`escapeData == true` does *not* escape anything.** It only inserts a `"` before and after the
payload (`handler_sse.c:181` and `:196`). There is **zero** backslash escaping, quote doubling, or
newline folding anywhere in the SSE path.

Every live call site passes `true`. The only `false` is in dead code — `sse_keepAlive()`
(`src/handler_sse.c:208-211`) is **never called** and would emit invalid JSON
(`"data":` with no value). The real keep-alive bypasses `sse_sendEvent` entirely.

Consequences for a client (payloads come from `tonies.json` / RTNL — untrusted strings):

| Payload content | Result on the wire | Client impact |
|---|---|---|
| `"` (e.g. title `Die "Biene" Maja`) | `data: { "type":"ContentTitle", "data":"Die "Biene" Maja" }` | `JSON.parse` **throws** |
| `\` | `"C:\path"` | invalid JSON escape → parse error |
| LF | SSE line terminated early | **frame desync** + truncated event |
| CR | same class | desync |
| control chars / invalid UTF-8 | invalid JSON | parse error |
| `rtnl-raw-log2` / `-log3` payloads | **safe by construction** — pure hex + interpolated numbers, never raw text | — |

⇒ **`JSON.parse(e.data)` must be wrapped in try/catch** and a failure treated as a dropped event. A
"relaxed" parser will not help — the data is genuinely ambiguous, not merely sloppy. Extract the
payload with a regex if you need to survive hostile titles.

There *is* a string escaper in the tree — `escapeString()` (`src/handler_rtnl.c:22-83`) — but it is
used only for the human-readable CSV log (`:568`, `:587`) and is itself lossy (replaces every
non-alphanumeric byte with `.`).

### 3.4 `data` is a string *or* a nested object

The envelope is always exactly two keys:

```json
{ "type": "<same as the event: name>", "data": <string | object> }
```

| Producer | Type of `data` |
|---|---|
| `sse_sendEvent(..., escapeData=true)` — all events except the two raw logs | **JSON string** |
| `rtnl-raw-log2` / `rtnl-raw-log3` (raw API, `handler_rtnl.c:209-248` / `:253-259`) | **nested JSON object** |
| `keep-alive` (polling loop, `handler_sse.c:83`) | **empty string** `""` |

### 3.5 No `id:`, no `retry:`, no comments

- The strings `id:` and `retry:` appear in **no** writer. The only field lines produced are
  `event: ` and `data: `.
- **No `Last-Event-ID` support**: the handler ignores the request entirely (`handler_sse.c:9`) and
  no replay buffer exists. Events are pushed to whichever slots are `active` at write time and are
  **never stored**.
- **No heartbeat comment lines** (`:…`); liveness is carried by the `keep-alive` *event*.
- ⇒ A reconnect always starts cold. **Your controller must be level-triggered / idempotent** and
  re-read authoritative state after every reconnect.

---

## 4. Connection lifecycle, limits, concurrency

### 4.1 Constants

```c
// include/handler_sse.h:15-17
#define SSE_MAX_CHANNELS 8
#define SSE_TIMEOUT_S 60
#define SSE_KEEPALIVE_S 15
```

```c
// include/handler_sse.h:19-26
typedef struct
{
    bool active;
    error_t error;
    HttpConnection *connection;
    time_t lastConnection;
    uint8_t channel;
} SseSubscriptionContext;
```

Global state `src/handler_sse.c:6-7` (`sseSubs[8]`, `sseSubscriptionCount`), initialised by
`sse_init()` (`:101-110`) called from `server_init()` (`src/server.c:887`).

### 4.2 Polling loop and termination

```c
// src/handler_sse.c:60-99 (condensed)
    time_t last = 0;
    while (true)
    {
        mutex_lock(MUTEX_SSE_CTX);
        if (sseCtx->error != NO_ERROR || sseCtx->active == FALSE || (sseCtx->lastConnection + SSE_TIMEOUT_S < time(NULL)))
        {
            httpFlushStream(connection);
            sseCtx->active = FALSE;
            ...
            break;
        }

        time_t now = time(NULL);
        if (now - last > SSE_KEEPALIVE_S)
        {
            mutex_lock(MUTEX_SSE_EVENT);
            sseCtx->error = httpWriteString(connection, "event: keep-alive\ndata: { \"type\":\"keep-alive\", \"data\":\"\" }\n\n");
            mutex_unlock(MUTEX_SSE_EVENT);
            last = now;
            ...
            sseCtx->lastConnection = now;
        }

        mutex_unlock(MUTEX_SSE_CTX);
        osDelayTask(100);
    }
```

- **Loop period: 100 ms.** Each subscriber thread wakes 10×/s and takes the global `MUTEX_SSE_CTX`
  10×/s.
- **Termination is effectively only via a write error.** `SSE_TIMEOUT_S` is *dead* in practice:
  `lastConnection` is refreshed on every successful write (`:153`) and every keep-alive (`:91`), so
  it can never exceed 60 s of silence. Commit `7be16fa` ("only update sse time when no error")
  deliberately made this stricter.
- **Slots can be held for minutes** if a client vanishes without FIN/RST (NAT/LB drop):
  `TCP_KEEP_ALIVE_SUPPORT` is disabled (`include/net_config.h:197`) and the server never reads
  during the stream. Recover slots by closing cleanly.
- **Task model:** one pthread per HTTP connection (`src/cyclone/common/os_port_posix.c:86-104`,
  created in `httpServerStart`). The SSE handler **blocks its connection thread for the entire
  stream lifetime** ⇒ each subscriber permanently consumes one of the 31 connection slots
  (`APP_HTTP_MAX_CONNECTIONS = 32` at `src/server.c:71-74`, `maxConnections = 31` at `:909`).

### 4.3 Keep-alive / heartbeat

- Template (`src/handler_sse.c:83`, single write, single chunk):
  `event: keep-alive\ndata: { "type":"keep-alive", "data":"" }\n\n`
- `time_t last = 0` ⇒ the **first keep-alive fires on the very first loop iteration** (~0–100 ms
  after the 200), then whenever `now - last > 15`. With 1-second `time()` granularity and a 100 ms
  poll the effective period is **~16 s**.
- Because it carries `event: keep-alive`, **`onmessage` never fires for it** — you must register a
  named listener.
- This is the **only** liveness signal: nothing else guarantees bytes on an idle box.

### 4.4 Log lines (default level 4 = INFO, `src/settings.c:63`)

```
SSE Client connected in slot %u in total %u clients          (handler_sse.c:46)
All slots full, in total %u clients                          (handler_sse.c:29)
SSE Client disconnected from slot %u, %u clients left        (handler_sse.c:70)
SSE Client with error %s                                    (handler_sse.c:73)
Failed to send header                                       (handler_sse.c:56)
```

---

## 5. Complete event registry

**18 distinct names.** Exhaustive — nothing declared-but-unfired except the *function*
`sse_keepAlive()` (not a new name). The only three files that emit SSE are `src/handler_sse.c`,
`src/toniebox_state.c`, and `src/handler_rtnl.c`.

### 5.1 Transport

| `event:` | `data` | Emitted at |
|---|---|---|
| `keep-alive` | `""` (empty string) | `src/handler_sse.c:83` — polling loop, hard-coded frame |

### 5.2 Box state — `src/toniebox_state.c`

| `event:` | `data` values | Emitted at |
|---|---|---|
| `TagValid` | 16-char uppercase hex uid | `toniebox_state.c:36` |
| `TagInvalid` | same | `toniebox_state.c:36` |
| `knock` | `"forward"` \| `"backward"` | `toniebox_state.c:60` |
| `tilt` | `"forward"` \| `"backward"` | `toniebox_state.c:65` |
| `playback` | `"starting"` `:74`, `"started"` `:77`, `"stopped"` `:89` | `toniebox_state.c:74,77,89` |
| `cmd` | `"stop"` `:152`, `"vol_limit_spk"` `:172`, `"vol_limit_hdp"` `:189`, `"led"` `:206`, `"slap_enabled"` `:223`, `"slap_dir"` `:240` | `toniebox_state.c:152,172,189,206,223,240` |

### 5.3 RTNL — `src/handler_rtnl.c`

| `event:` | `data` | Emitted at |
|---|---|---|
| `rtnl-raw-log2` | **nested object** | `handler_rtnl.c:209-248` |
| `rtnl-raw-log3` | **nested object** | `handler_rtnl.c:253-259` |
| `pressed` | `"ear-big-double"`, `"ear-big"`, `"ear-small-double"`, `"ear-small"` | `handler_rtnl.c:272, 278, 287, 293` |
| `charger` | `"on"`, `"off"` | `handler_rtnl.c:310, 314` |
| `ContentAudioId` | decimal `audioId` (string) | `handler_rtnl.c:357` |
| `ContentTitle` | `"Unknown"` `:372` or `item->title` `:393` | `handler_rtnl.c:372, 393` |
| `ContentPicture` | raw `item->picture` (may be relative `/cache/…`) | `handler_rtnl.c:395` |
| `BoxTilt-A` | signed decimal int32 | `handler_rtnl.c:407` |
| `BoxTilt-B` | signed decimal int32 | `handler_rtnl.c:414` |
| `VolumeLevel` | signed decimal int32, documented 0–16 | `handler_rtnl.c:426` |
| `VolumedB` | signed decimal int32, negative dB | `handler_rtnl.c:429` |

`rtnl-raw-log2` / `rtnl-raw-log3` are by far the highest volume — they fire for **every** RTNL
message, unfiltered (`handler_rtnl.c:134-139`).

---

## 6. Event groups in detail

### 6.1 Transport / heartbeat

| | |
|---|---|
| **Payload** | `{ "type":"keep-alive", "data":"" }` |
| **Trigger** | `now - last > SSE_KEEPALIVE_S` (15 s) in the subscriber loop |
| **Cadence** | immediately on connect, then every ~16 s |
| **Emit site** | `src/handler_sse.c:83` (hard-coded string, not via `sse_sendEvent`) |
| **Use** | liveness watchdog. If nothing for ≳30 s → connection is dead, reconnect. |
| **Gotcha** | `sse_keepAlive()` at `:208-210` is **dead code** and would emit invalid JSON. |

### 6.2 Tag & playback lifecycle

Emission sites (all 8 call sites in the tree, [P]):

```
src/toniebox_state.c:36   sse_sendEvent(valid ? "TagValid" : "TagInvalid", cuid, true);
src/toniebox_state.c:74   sse_sendEvent("playback", "starting", true);
src/toniebox_state.c:77   sse_sendEvent("playback", "started", true);
src/toniebox_state.c:89   sse_sendEvent("playback", "stopped", true);
src/handler_rtnl.c:357    sse_sendEvent("ContentAudioId", str_buf, true);
src/handler_rtnl.c:372    sse_sendEvent("ContentTitle", "Unknown", true);
src/handler_rtnl.c:393    sse_sendEvent("ContentTitle", item->title, true);
src/handler_rtnl.c:395    sse_sendEvent("ContentPicture", item->picture, true);
```

#### `TagValid` / `TagInvalid`

```c
// src/toniebox_state.c:24-40
void tbs_tag_placed(client_ctx_t *client_ctx, uint64_t uid, bool valid)
{
    client_ctx->state->tag.uid = uid;
    client_ctx->state->tag.valid = valid;
    if (valid)
    {
        setLastUid(client_ctx->state->tag.uid, client_ctx->settingsNoOverlay);
    }

    char cuid[16 + 1];
    osSprintf((char *)cuid, "%016" PRIX64 "", (int64_t)uid);

    sse_sendEvent(valid ? "TagValid" : "TagInvalid", cuid, true);

    mqtt_sendBoxEvent(valid ? "TagValid" : "TagInvalid", cuid, client_ctx);
    mqtt_sendBoxEvent(!valid ? "TagValid" : "TagInvalid", "", client_ctx);
}
```

- **Value:** `"%016" PRIX64` — 16 chars, zero-padded, **uppercase** hex, no `0x`. Buffer is
  `char cuid[17]`, so no truncation.
- **Source decode:** the uid arrives as 8 raw bytes read via `read_big_endian64`
  (`src/handler_rtnl.c:165-168`), which is *two big-endian 32-bit words, low word first* — a
  half-word byte swap, not a plain 64-bit BE read — and which **sign-extends** the first word
  (§11.1). Expect `FFFFFFFF…` payloads for real tags whose first byte is ≥ `0x80`.
- **Trigger** (`src/handler_rtnl.c:336-350`):

  | grp | fn | constant | effect |
  |---|---|---|---|
  | 15 | 8627 | `RTNL2_FUNC_TAG_VALID_CC3200` | **SSE `TagValid`** |
  | 15 | 16065 | `RTNL2_FUNC_TAG_VALID_ESP32` | **SSE `TagValid`** |
  | 15 | 8646 | `RTNL2_FUNC_TAG_INVALID_CC3200` | **SSE `TagInvalid`** |
  | 15 | 15452 | `RTNL2_FUNC_TAG_INVALID_ESP32` | **SSE `TagInvalid`** |
  | 15 | 8630 | `RTNL2_FUNC_TAG_VALID_REMOVED_CC3200` | **no SSE** (MQTT only) |
  | 15 | 16067 | `RTNL2_FUNC_TAG_VALID_REMOVED_ESP32` | **no SSE** (MQTT only) |

- **Validity is the box's own verdict, verbatim.** There is no cache test, no `tonies.json` lookup,
  no CFD round trip, no hash. `TagValid` means "the box claims this tag is known", nothing more.

- **uid → ruid** (needed for `/api/getTagInfo` and content paths), from `getContentPathFromUID`
  (`src/handler.c:647-658`): each 8-char half is byte-reversed.

  ```
  uid  = "1122334455667788"
  ruid = "88776655" + "44556677"   ->  <contentdir>/88776655/44556677
  ```

  This operates on the hex **string**, so it is unaffected by the sign-extension bug — but it is
  applied to the already-mangled uid, so for a sign-extended uid you get
  `88776655` + `44556677` from `1122334455667788` and `D5E6F708` + `…` from `FFFFFFFFD5E6F708`.

#### `playback`

```c
// src/toniebox_state.c:69-102
void tbs_playback(client_ctx_t *client_ctx, toniebox_state_playback_t playback)
{
    switch (playback)
    {
    case TBS_PLAYBACK_STARTING:
        sse_sendEvent("playback", "starting", true);
        mqtt_sendBoxEvent("Playback", "OFF", client_ctx);
    case TBS_PLAYBACK_STARTED:                 // <-- NO break
        sse_sendEvent("playback", "started", true);
        mqtt_sendBoxEvent("Playback", "ON", client_ctx);
        break;
    case TBS_PLAYBACK_STOPPED:
        if (client_ctx->state->box.stream_ctx.stop_on_playback_stop && !client_ctx->state->box.stream_ctx.quit && client_ctx->state->tag.valid)
        {
            client_ctx->state->box.stream_ctx.active = false;
        }
        client_ctx->state->tag.audio_id = 0;
        client_ctx->state->tag.valid = false;
        client_ctx->state->tag.uid = 0;

        sse_sendEvent("playback", "stopped", true);
```

- **Values:** three string literals `"starting"` / `"started"` / `"stopped"`.
- **Trigger** (`src/handler_rtnl.c:317-325`): log3 `field2` = 11 / 12 / 13
  (`RTNL3_TYPE_PLAYBACK_STARTING/STARTED/STOPPED`).
- **[P] The missing `break`:** `TBS_PLAYBACK_STARTING` **falls through** to
  `TBS_PLAYBACK_STARTED`. A single log3 `field2=11` emits `playback:starting` **immediately
  followed by** `playback:started`, back-to-back. A subsequent `field2=12` then emits a *second,
  redundant* `playback:started`. ⇒ **`starting` carries zero information.** Ignore it, or treat it
  as "imminent"; latch on `started`. Never count occurrences.
- `TBS_PLAYBACK_STOPPED` has **two** RTNL sources:
  1. log3 `field2 = 13`;
  2. log2 `22-6480` (`RTNL2_FUNC_AUDIO_PLAY`) with a **system-sound** path
     `content/0000000X/0000000Y` → `tbs_playback_file` → `tbs_playback_system_sound`
     (`src/toniebox_state.c:104-139`). `00000006` is
     `TBS_SYS_SOUND_TONIE_PLAYBACK_FINISHED` (`include/toniebox_state_type.h:11`) but so are
     charger-plug, low-battery, install hints. ⇒ **`playback:stopped` is not a reliable
     "end of queue" marker.**
- **Side effect:** `TBS_PLAYBACK_STOPPED` is the **only** state-clearing path — it zeroes
  `tag.audio_id`, `tag.valid`, `tag.uid`. That means the model-fallback lookup in the next
  `ContentAudioId` fails, so you will see `"Unknown"` for the first content report after any stop.

#### `ContentAudioId` / `ContentTitle` / `ContentPicture`

```c
// src/handler_rtnl.c:351-400 (condensed)
else if ((rpc->log2->function_group == RTNL2_FUGR_AUDIO_A && (rpc->log2->function == RTNL2_FUNC_AUDIO_ID_CC3200 || rpc->log2->function == RTNL2_FUNC_AUDIO_ID_ESP32)) || (rpc->log2->function_group == RTNL2_FUGR_AUDIO_B && rpc->log2->function == RTNL2_FUNC_AUDIO_ID))
{
    uint32_t audioId = read_little_endian32(rpc->log2->field6.data);
    client_ctx->state->tag.audio_id = audioId;
    osSprintf(str_buf, "%u", audioId);
    toniesJson_item_t *item = tonies_byAudioId(audioId);
    sse_sendEvent("ContentAudioId", str_buf, true);

    if (item == NULL || audioId == SPECIAL_AUDIO_ID_ONE)
    {
        tonie_info_t *tonieInfo = getTonieInfoFromUid(client_ctx->state->tag.uid, false, client_ctx->settings);
        if (tonieInfo->valid || (tonieInfo->json._valid && tonieInfo->json.tonie_model != NULL))
        {
            item = tonies_byModel(tonieInfo->json.tonie_model);
        }
        freeTonieInfo(tonieInfo);
    }

    if (item == NULL)
    {
        sse_sendEvent("ContentTitle", "Unknown", true);
        mqtt_sendBoxEvent("ContentTitle", "Unknown", client_ctx);
        if (audioId < TEDDY_BENCH_AUDIO_ID_DEDUCT)
        {
            char *url = custom_asprintf("%s/img_custom.png", settings_get_string("core.host_url"));
            mqtt_sendBoxEvent("ContentPicture", url, client_ctx);   // MQTT ONLY
            osFreeMem(url);
        }
        else
        {
            char *url = custom_asprintf("%s/img_unknown.png", settings_get_string("core.host_url"));
            mqtt_sendBoxEvent("ContentPicture", url, client_ctx);   // MQTT ONLY
            osFreeMem(url);
        }
    }
    else
    {
        char *url = absolute_url(item->picture);

        sse_sendEvent("ContentTitle", item->title, true);
        mqtt_sendBoxEvent("ContentTitle", item->title, client_ctx);
        sse_sendEvent("ContentPicture", item->picture, true);        // raw, NOT absolute_url()
        mqtt_sendBoxEvent("ContentPicture", url, client_ctx);

        osFreeMem(url);
    }
}
```

| | |
|---|---|
| **Trigger** | grp 20 fn 6803/16096, or grp 22 fn 297 — a **box-initiated** report |
| **`ContentAudioId` format** | `"%u"` — unsigned base-10 decimal, **as a string** |
| **Source decode** | `read_little_endian32(field6.data)` — LE `uint32` |
| **Trigger ordering** | `ContentAudioId` → `ContentTitle` → `ContentPicture`, always in that order, same RTNL message, atomic w.r.t. other SSE events (`MUTEX_SSE_EVENT`) |

- `ContentTitle` **always** follows `ContentAudioId` — it is structurally impossible otherwise
  (only two call sites, both inside this branch).
- `ContentPicture` fires **only** in the `item != NULL` branch. **In the `"Unknown"` case there is
  no `ContentPicture` on SSE at all** — the `img_custom.png` / `img_unknown.png` fallbacks at
  `:377` / `:384` are `mqtt_sendBoxEvent` only.
- `ContentPicture`'s SSE payload is the **unresolved** `item->picture`, unlike MQTT which gets
  `absolute_url()`. With `tonie_json.cache_images = true` it is a relative `/cache/<sha256>.<ext>`
  (`src/cache.c:197`); otherwise it is the raw upstream URL from the tonie JSON `"pic"` field.
  **Resolve it against your own base URL.**

#### Full placement sequence

Within one `handleRtnl` POST, RPCs are processed in wire order (`:95-141`); within one RPC,
**log3 dispatch precedes log2 dispatch** (`:262` before `:332`).

| # | RTNL input | SSE events, in order |
|---|---|---|
| 1 | log2 15-16065 / 15-8627 | `rtnl-raw-log2` → **`TagValid`** |
| 2 | log2 20-16096 / 20-6803 / 22-297 | `rtnl-raw-log2` → **`ContentAudioId`** → **`ContentTitle`** → **`ContentPicture`** (if resolved) |
| 3 | log3 field2 = 11 | `rtnl-raw-log3` → **`playback:starting`** → **`playback:started`** |
| 4 | log3 field2 = 12 | `rtnl-raw-log3` → **`playback:started`** (duplicate) |
| 5 | next track (multi-track TAF) | back to step 2 with a new `ContentAudioId` |
| 6 | end of queue | log3 13 **or** log2 22-6480 `content/00000000/00000006` → **`playback:stopped`** |

> `ContentAudioId` normally arrives **before** `playback:starting` (the box announces the audio id
> it is about to play first), but this is box-driven, not enforced. **Invariant:** the three
> `Content*` events are always emitted together, in that order, atomically.

#### Tag removal sequence

| # | RTNL input | SSE events |
|---|---|---|
| 1 | log2 15-16067 / 15-8630 | `rtnl-raw-log2` **only** |
| 2 | log3 field2 = 13, **or** log2 22-6480 system sound | **`playback:stopped`** |

**There is no SSE event for tag removal.** `tbs_tag_removed()` (`src/toniebox_state.c:42-56`)
contains no `sse_sendEvent` at all. The only unambiguous removal signal anywhere is on **MQTT**: the
payload `-<cuid>` (leading minus, `:52`) on `{mqtt.topic}/box/{cn}/TagValid`.

#### The state machine

`toniebox_state_t` (`include/toniebox_state_type.h:39-70`) — one instance per overlay
(`MAX_OVERLAYS = 16 + 1`, `include/settings.h:30`), zero-initialised at boot
(`static toniebox_state_t Box_State_Overlay[MAX_OVERLAYS];` at `src/toniebox_state.c:5`).

| Function (site) | State mutated | SSE event |
|---|---|---|
| `tbs_tag_placed(uid, true)` `:24` | `tag.uid`, `tag.valid=true`, `setLastUid` | `TagValid` |
| `tbs_tag_placed(uid, false)` | `tag.uid`, `tag.valid=false` | `TagInvalid` |
| `tbs_tag_removed(uid, true)` `:42` | `tag.uid`, `tag.valid=**true**`, `setLastUid` | **none** (MQTT `-<cuid>`) |
| `tbs_knock(ctx, fwd)` `:58` | none | `knock` |
| `tbs_tilt(ctx, fwd)` `:63` | none | `tilt` |
| `tbs_playback(STARTING)` `:73` | **none** | `playback` ×2 (fall-through) |
| `tbs_playback(STARTED)` `:76` | **none** | `playback` |
| `tbs_playback(STOPPED)` `:80` | `stream_ctx.active=false`; `tag.*=0` | `playback:stopped` |
| `tbs_playback_system_sound` `:136` | as STOPPED | `playback:stopped` |
| `tbs_cmd_stop(overlay)` `:141` | `stream_ctx.active=false` | `cmd:stop` |
| `tbs_cmd_set_*` `:159-242` | settings only | `cmd:*` |
| audio-id branch `:353` | `tag.audio_id` (write-only) | `ContentAudioId` + `ContentTitle` (+ `ContentPicture`) |

**Critically: there is no stored playback state.** `toniebox_state_playback_t` is only a function
parameter (`include/toniebox_state.h:19`); nothing latches "currently playing". A fresh process —
or a reconnecting client — has **zero** in-memory record of whether a box is playing.

Dead fields: `box.playback`, `box.charger`, `box.volumeLevel`, `box.volumedB`, `tag.custom` are
**never written anywhere**. `tag.audio_id` is write-only. `box.id` / `box.name` are MQTT-only.

#### Multi-box attribution

**None of these events carries a box identifier.** `sse_sendEvent()` has **no `client_ctx_t`
parameter** (`include/handler_sse.h:30`) and `sse_rawData` fans bytes out to every active slot
(`handler_sse.c:143-155`). `handleApiSse`'s own `ctx` is unused (`:9`).

⇒ With N boxes, every subscriber sees the interleaved union of all N boxes' events and **cannot
tell them apart**. `rtnl-raw-log2` carries `uptime` (restarts on reboot) and `sequence` as soft
fingerprints only. **MQTT is properly attributed** (`{mqtt.topic}/box/{box_cn}`, `src/mqtt.c:982`).

**If more than one box exists, subscribe per box to MQTT, or run one teddyCloud per box.**

### 6.3 Physical interaction & box status

#### `knock` / `tilt`

```c
58: void tbs_knock(client_ctx_t *client_ctx, bool forward)
59: {
60:     sse_sendEvent("knock", forward ? "forward" : "backward", true);
61:     mqtt_sendBoxEvent(forward ? "KnockForward" : "KnockBackward", "{\"event_type\": \"triggered\"}", client_ctx);
62: }
```
```c
63: void tbs_tilt(client_ctx_t *client_ctx, bool forward)
64: {
65:     sse_sendEvent("tilt", forward ? "forward" : "backward", true);
66:     mqtt_sendBoxEvent(forward ? "TiltForward" : "TiltBackward", "{\"event_type\": \"triggered\"}", client_ctx);
67: }
```

- **Values:** exactly two literals each, lowercase.
- **Trigger** (`src/handler_rtnl.c:297-308`): log3 `field2` = 3/4 (knock), 5/6 (tilt).
- **Direction is decided by the box, not by teddyCloud.** No threshold, no sign test, no
  hysteresis anywhere in teddyCloud. `"forward"` = the box's own gesture classifier fired forward.
  You cannot re-derive it from `BoxTilt-A/B`.

#### `pressed`

```c
262:     if (rpc->log3)
263:     {
264:         settings_internal_rtnl_t *rtnl_setting = &client_ctx->settings->internal.rtnl;
265:         switch (rpc->log3->field2)
266:         {
267:         case RTNL3_TYPE_EAR_BIG:
268:             if (rtnl_setting->lastEarId == EAR_BIG && rtn_setting->wasDoubleEarpress)
269:             {
270:                 rtn_setting->lastEarId = EAR_NONE;
271:                 rtn_setting->wasDoubleEarpress = false;
272:                 sse_sendEvent("pressed", "ear-big-double", true);
...
278:                 sse_sendEvent("pressed", "ear-big", true);
...
287:                 sse_sendEvent("pressed", "ear-small-double", true);
...
293:                 sse_sendEvent("pressed", "ear-small", true);
```

- **Values (4 total):** `"ear-big"`, `"ear-big-double"`, `"ear-small"`, `"ear-small-double"`.
- **Trigger:** log3 `field2` = 1 (big ear) / 2 (small ear).
- **Double-press detection is a teddyCloud heuristic across two RTNL messages:**
  1. A grp-27 volume-change message arms it (`src/handler_rtnl.c:432-437`):
     ```c
     if (rpc->log2->uptime - rtn_setting->lastEarpress < rtn_setting->multipressTime)
     {
         rtn_setting->wasDoubleEarpress = true;
     }
     rtn_setting->lastEarpress = rpc->log2->uptime;
     ```
     `multipressTime` default **300** (`src/settings.c:202`), interpreted as milliseconds of the
     box's own `uptime`.
  2. The following log3 press message is classified as double if `lastEarId` matches *and*
     `wasDoubleEarpress` — then both flags are **reset**.
- **Weakness [P]:** if the box does not report a volume change for a press (volume at 0 or 16,
  muted, firmware variant), the second press is **not** recognised as a double — you get two
  `ear-big` events. Conversely two grp-27 messages for one physical press inject a false double.
  Treat `-double` as best-effort.
- **No triple state** exists — after a double, `lastEarId = EAR_NONE`. The HA `triple-pressed`
  event type advertised at `src/mqtt.c:1010,1018` and `MQTT_CONTROL.md:56-57` is **dead**.

#### `charger`

```c
309:         case RTNL3_TYPE_CHARGER_ON:
310:             sse_sendEvent("charger", "on", true);
311:             mqtt_sendBoxEvent("Charger", "ON", client_ctx);
312:             break;
313:         case RTNL3_TYPE_CHARGER_OFF:
314:             sse_sendEvent("charger", "off", true);
315:             mqtt_sendBoxEvent("Charger", "OFF", client_ctx);
316:             break;
```

- **Values:** `"on"` / `"off"` — **lowercase on SSE**, **uppercase `ON`/`OFF` on MQTT**. Normalise
  case if cross-checking.
- **Trigger:** log3 `field2` = 7 / 8. No CC/ESP split.
- It is the **box's real physical state**, not a request — `toniebox_state_box_t.charger` is never
  assigned anywhere, so nothing in teddyCloud can write it.
- **Edge-only, no readback.** If disconnected while charging you get no event and no way to query
  the current value.

#### `BoxTilt-A` / `BoxTilt-B`

```c
401:         else if (rpc->log2->function_group == RTNL2_FUGR_TILT)
402:         {
403:             if (rpc->log2->function == RTNL2_FUNC_TILT_A_ESP32)
404:             {
405:                 int32_t angle = read_little_endian32(rpc->log2->field6.data);
406:                 osSprintf(str_buf, "%d", angle);
407:                 sse_sendEvent("BoxTilt-A", str_buf, true);
...
414:                 sse_sendEvent("BoxTilt-B", str_buf, true);
```

- **Format:** `"%d"` — signed decimal, offset **0** of `field6`, LE `int32`.
- **Units: undefined in this codebase.** No degree conversion, no `/100`, no clamp, no
  `unit_of_meas` on the HA entity (`src/mqtt.c:1029-1034`), no entry in `MQTT_CONTROL.md`. The
  author's `//?` on the function codes (`include/handler_rtnl.h:78-79`) is the honest state of
  knowledge. Treat as opaque telemetry.
- **ESP32 only** — no CC3200 tilt codes exist, so a Toniebox 1 **never** emits these. It does
  still emit the `tilt` gesture event.
- **Unrelated to the `tilt` event** — different quantities from different messages (§6.3 above).
- Both publish to the **same** MQTT topic `…/BoxTilt` (`:408`, `:415`), so **SSE is the only way to
  distinguish the A and B channels**.
- Continuous, no dedup, no change detection. **Use `tilt`, not `BoxTilt-*`.**

#### `VolumeLevel` / `VolumedB`

```c
418:         else if (rpc->log2->function_group == RTNL2_FUGR_VOLUME)
419:         {
420:             if (rpc->log2->function == RTNL2_FUNC_VOLUME_CHANGE_CC3200 || rpc->log2->function == RTNL2_FUNC_VOLUME_CHANGE_ESP32)
421:             {
422:                 /* DE210000 DBFFFFFF 01000000 */ /* 963C0000 D8FFFFFF 00000000 */
423:                 int32_t volumedB = read_little_endian32(&rpc->log2->field6.data[4]);
424:                 int32_t volumeLevel = read_little_endian32(&rpc->log2->field6.data[8]);
425:                 osSprintf(str_buf, "%d", volumeLevel);
426:                 sse_sendEvent("VolumeLevel", str_buf, true);
...
429:                 sse_sendEvent("VolumedB", str_buf, true);
```

**Payload layout (12 bytes):**

| Bytes | Meaning | Read as |
|---|---|---|
| `[0..3]` | **ignored / unknown** (observed 8670, 15478) | — |
| `[4..7]` | `VolumedB` | signed LE int32, offset 4 |
| `[8..11]` | `VolumeLevel` | signed LE int32, offset 8 |

Decoding the two captured samples in the comment:
- `DE 21 00 00 | DB FF FF FF | 01 00 00 00` → `VolumedB = -37`, `VolumeLevel = 1`
- `96 3C 00 00 | D8 FF FF FF | 00 00 00 00` → `VolumedB = -40`, `VolumeLevel = 0`

- **Order is fixed:** `VolumeLevel` then `VolumedB`, always as a pair, from one RTNL message.
- **`VolumeLevel` range: 0–16** (`MQTT_CONTROL.md:18`, `:168`). teddyCloud hard-codes no bound —
  validate `0 <= v <= 16` yourself, since a corrupt read can yield a negative.
- **`VolumedB` unit:** dB (only declared on the HA entity, `src/mqtt.c:1087`).
  `MQTT_CONTROL.md:32` documents no numeric range. *Inference, not code:* `VolumedB ≈ -40 + 3 ×
  VolumeLevel` from the two samples — capture your own box before relying on it.
- **No length check on `field6`** (`handler_rtnl.c:423-424` read offset 8 unconditionally) ⇒ a
  short payload is an out-of-bounds read in teddyCloud. Be tolerant of absurd values.
- **Read-only.** No protocol path sets the actual volume.

#### Ordering within one RTNL message

`rtnlEvent()` executes four blocks in fixed order (`src/handler_rtnl.c:203-493`):

1. `:207-249` — `rtnl-raw-log2`
2. `:251-260` — `rtnl-raw-log3`
3. `:262-330` — log3 semantics → `pressed`, `knock`, `tilt`, `charger`, `playback`
4. `:332-492` — log2 semantics → `Content*`, `BoxTilt-*`, `VolumeLevel`, `VolumedB`

A single RTNL RPC may carry both log2 and log3 ⇒ **an ear press arrives as `pressed` FIRST, then
`VolumeLevel`, then `VolumedB`** — not the reverse. If your state machine reacts to `pressed` by
immediately wanting the new volume, wait for the two following frames; they are guaranteed to come
after it in the same write burst.

#### Reliability summary

| Event | Edge- or level-triggered | Safe to drive a state machine from? |
|---|---|---|
| `knock`, `tilt`, `pressed`, `charger` | edge, box-classified | **yes** (with the `pressed -double` caveat) |
| `VolumeLevel`, `VolumedB` | per message, **no dedup** | yes, as a level read |
| `BoxTilt-A/B` | continuous stream, no dedup | no — telemetry only |
| `playback:started` | edge, but fires 1–2× per session and again on track change | yes, as a **latch** |
| `playback:stopped` | edge, but ambiguous | as a "possibly stopped" hint |
| `ContentAudioId` | box-initiated report | weak for streams |

### 6.4 Command acknowledgements (`cmd`)

**The `cmd` events are acknowledgements, not commands.** They are emitted at the *end* of each
`tbs_cmd_*` function, after the real work. All six call sites in the entire repository are MQTT
receive callbacks:

| `cmd` value | Sole caller | `src/mqtt.c` | Topic |
|---|---|---|---|
| `stop` | `mqtt_box_cmd_stop_rx` → `tbs_cmd_stop` | `:834` | `…/CmdStop/set` (`:1184`) |
| `vol_limit_spk` | `mqtt_box_cmd_vol_spk_rx` | `:846` | `…/CmdVolLimitSpk/set` (`:1193`) |
| `vol_limit_hdp` | `mqtt_box_vol_hdp_rx` | `:871` | `…/CmdVolLimitHdp/set` (`:1209`) |
| `led` | `mqtt_box_cmd_led_rx` | `:896` | `…/CmdLed/set` (`:1225`) |
| `slap_enabled` | `mqtt_box_cmd_slap_en_rx` | `:921` | `…/CmdSlapEnabled/set` (`:1241`) |
| `slap_dir` | `mqtt_box_cmd_slap_dir_rx` | `:944` | `…/CmdSlapDirection/set` (`:1254`) |

**No HTTP path to these functions exists.** Details, argument ranges and enforcement in
[§9](#9-control-channel--how-to-actually-drive-the-box).

Critical properties:

- **No argument is echoed** — `sse_sendEvent("cmd", "led", true)` carries only the command *name*.
  Remember what you sent, or read the setting back via REST.
- **`cmd` is emitted only on success** — the `sse_sendEvent` call sits *after* the validation
  `if (…) return false;`. Out-of-range values and a missing box config are **silently dropped and
  logged** (`MQTT_CONTROL.md:143`) with **no SSE frame at all**.
- `CmdStop` emits `cmd:stop` only if a stream was actually active (`toniebox_state.c:148`).
  **Silence means "nothing to stop", not "acknowledged".**
- The five settings commands reach the box only at its **next freshness check**
  (`setTonieboxSettings`, `src/handler.c:660-672`, called from `src/handler_cloud.c:1100`).

### 6.5 Raw RTNL feed

#### Emission

```c
// src/handler_rtnl.c:207-260 (structure)
if (rpc->log2)
{
    sse_startEventRaw("rtnl-raw-log2");
    osSprintf(buffer, "{\"uptime\": %" PRIu64 ", \"sequence\": %" PRIu32 ", \"field3\": %" PRIu32
                      ", \"function_group\": %" PRIu32 ", \"function\": %" PRIu32 ", \"field6\": \"", ...);
    sse_rawData(buffer);
    if (rpc->log2->field6.len > 0) { /* uppercase hex */ sse_rawData(buffer); }
    osSprintf(buffer, "\",\"field8\": %" PRIu32 ", \"field9\": \"", rpc->log2->field8);
    sse_rawData(buffer);
    if (rpc->log2->field9.len > 0) { /* uppercase hex */ sse_rawData(buffer); }
    sse_rawData("\"}");
    sse_endEventRaw();
}

if (rpc->log3)
{
    sse_startEventRaw("rtnl-raw-log3");
    osSprintf(buffer, "{\"datetime\": %" PRIu32 ", \"field2\": %" PRIu32 "}", rpc->log3->datetime, rpc->log3->field2);
    sse_rawData(buffer);
    sse_endEventRaw();
}
```

**Exact wire bytes:**

```
event: rtnl-raw-log2\ndata: { "type":"rtnl-raw-log2", "data":{"uptime": <u64>, "sequence": <u32>, "field3": <u32>, "function_group": <u32>, "function": <u32>, "field6": "<HEX>", "field8": <u32>, "field9": "<HEX>"} }\n\n

event: rtnl-raw-log3\ndata: { "type":"rtnl-raw-log3", "data":{"datetime": <u32>, "field2": <u32>} }\n\n
```

The inner `}` (from `sse_rawData("\"}")` for log2, from the `sprintf` for log3) closes the nested
object; the ` }` from `sse_endEventRaw()` closes the envelope. The JSON is valid.

**Worked example — tag placement (ESP32), uid `0A0B0C0D00000002`:**

```
event: rtnl-raw-log2
data: { "type":"rtnl-raw-log2", "data":{"uptime": 13505, "sequence": 158, "field3": 0, "function_group": 15, "function": 16065, "field6": "000000020A0B0C0D", "field8": 0, "field9": ""} }

```

⚠️ Choose examples whose first word has the MSB clear — see the sign-extension hazard in
[§11.1](#111-read_big_endian64-sign-extends-and-collapses-uids). A payload such as
`D5E6F70801A2B3C4` does **not** yield `01A2B3C4D5E6F708`; it yields `FFFFFFFFD5E6F708`.

**Worked example — region setting (CC3200), from the source comment at `src/handler_rtnl.c:463`:**

```
event: rtnl-raw-log2
data: { "type":"rtnl-raw-log2", "data":{"uptime": 7606, "sequence": 102, "field3": 0, "function_group": 6, "function": 791, "field6": "45550030010000", "field8": 0, "field9": ""} }

```

`45550030010000` → `45 55 00 30 01 00 00` → `"EU\0…"`. The first four bytes are consumed as a
NUL-terminated C string by `settings_set_string_id()` at `:465`.

**Worked example — playback stopped:**

```
event: rtnl-raw-log3
data: { "type":"rtnl-raw-log3", "data":{"datetime": 1704804523, "field2": 13} }

```

#### Field reference

| JSON key | proto field | type | notes |
|---|---|---|---|
| `uptime` | 1, `required fixed64` | number | box uptime; **unit unresolved [E]** — seconds or ms |
| `sequence` | 2, `required uint32` | number | per-boot counter, reset on reboot. Useful for gap/reboot detection. |
| `field3` | 3, `required uint32` | number | **unknown**, never read anywhere |
| `function_group` | 4, `required uint32` | number | event category |
| `function` | 5, `required uint32` | number | event sub-type |
| `field6` | 6, `required bytes` | **UPPERCASE hex string** | primary payload, may be `""` |
| `field8` | 8, `optional fixed32` | number | **opaque**; emitted unconditionally, so absent and `0` are indistinguishable (`:232-235` does not check `has_field8`, unlike `rtnlEventLog` at `:530-533`) |
| `field9` | 9, `optional bytes` | **UPPERCASE hex string** | secondary payload; **no decoder exists in the repo** |
| `datetime` (log3) | 1, `required fixed32` | number | box wall clock; **format undocumented** — observed samples decode as packed BCD `[0x20YY][0xMM][0xDD][0x0H][0xmm][0xss]` [E] |
| `field2` (log3) | 2, `required uint32` | number | log3 event type |

Proto field **7 is unknown/reserved** and skipped by protobuf-c — that is why the JSON key order
jumps 6 → 8 → 9. `proto/toniebox.pb.rtnl.proto:1-24`.

#### Decoding the hex payloads

`field6` / `field9` are raw bytes with **no inherent endianness**; interpretation is
event-specific.

| Group | Bytes | Layout | Decoder |
|---|---|---|---|
| 15 (tag) | 8 | two BE 32-bit words, **low word first** (a half-word byte swap) | `read_big_endian64` `handler_rtnl.c:165-168` — **sign-extends**, see §11.1 |
| 12 (tilt) | 4 | signed LE int32 at offset 0 | `read_little_endian32` `:150-153` |
| 20/22 (audio id) | 4 | unsigned LE uint32 | `read_little_endian32` |
| 27 (volume) | 12 | `[0..3]` unused · `[4..7]` signed LE int32 dB · `[8..11]` signed LE int32 level | `:422-424` |
| 6-791, 6-1009, 37-1009, 22-6480, 8-703/704/7146 | var | NUL-terminated ASCII string | `settings_set_string_id(...)` / `tbs_playback_file` |

Tag UID swap — the legacy UI does exactly this (`contrib/data/www/legacy.html:1653,1661`):

```js
const rearrangedPayload = payload.slice(8) + payload.slice(0, 8);   // valid only when length === 16
```

```
payload hex:  00000002 0A0B0C0D
UID:          0A0B0C0D 00000002   ->  0A0B0C0D00000002
```

`read_big_endian64` swaps the two halves (the first four bytes become the **low** word) and
additionally sign-extends the first word from `int32_t`, so payloads beginning with a byte
≥ `0x80` come out as `FFFFFFFF…` and lose the low word entirely — §11.1.

`hexToStr` (`legacy.html:1602-1614`) is a *display* helper only — maps each byte to its ASCII
character for 32–126, else `.`. `45550030010000` → `EU.0...`.

#### Trigger conditions and volume

- Emitted **once per protobuf message containing a log2 / log3** (`handler_rtnl.c:134-139`).
  A message with both emits both, log2 first.
- Messages with neither are silently skipped — no SSE.
- **No filtering by function group, function, or subscriber.** Every raw event goes to every
  connected client.
- Emitted **regardless of the `rtnl.logRaw` setting** — that only controls writing the raw binary
  to a file (`:118-128`).
- **No fixed polling interval** — the box pushes when it has something to report. The three
  documented samples (uptime 7606 / 13505 / 19080) suggest roughly one message every 100–430 s in
  interactive use, with a dense burst at boot. Expect a handful per minute.
- **CPU cost is paid even with no SSE client attached** — `rtnlEvent()` does the `osSprintf` and
  hex loops before `sse_rawData` checks for active subscriptions.

#### What the raw feed gives you that the derived events do not

1. **Tag removal** — group 15, function **8630** (CC3200) or **16067** (ESP32). The *only* lossless
   tag-removal signal on SSE, because `function_group` and `function` are top-level JSON fields.
2. **Firmware identity** — group 8, functions 703 / 704 / 7146.
3. **Region and network domains** — groups 6 and 37.
4. **Unknown future codes** — unknown pairs fall through silently, so raw is forward-compatible
   where derived events are not.
5. **Transport metadata** — `sequence` (gap + reboot detection), `uptime`.

---

## 7. RTNL function-code tables

The numeric constants are **hand-written C enums** in `include/handler_rtnl.h` — not in the
`.proto` files and not in generated code. This is the authoritative source.

### 7.1 Function groups — `include/handler_rtnl.h:16-27`

```c
16: typedef enum
17: {
18:     RTNL2_FUGR_UNKNOWN3 = 3,
19:     RTNL2_FUGR_NETWORK_HTTP = 6,
20:     RTNL2_FUGR_FIRMWARE = 8,
21:     RTNL2_FUGR_TILT = 12,
22:     RTNL2_FUGR_TAG = 15,
23:     RTNL2_FUGR_AUDIO_A = 20,
24:     RTNL2_FUGR_AUDIO_B = 22,
25:     RTNL2_FUGR_VOLUME = 27,
26:     RTNL2_FUGR_NETWORK_TCP = 37,
27: } rtnl_log2_function_group;
```

### 7.2 `log2.function` → derived event

| Grp | Fn | Hex | Constant (`include/handler_rtnl.h`) | Derived SSE |
|---|---|---|---|---|
| 3 | 7004 | `0x1B5C` | `UNKNOWN_DURING_DWNLD` | — |
| 6 | 110 | `0x006E` | `NETWORK_HTTP_PATH` | — |
| 6 | 785 | `0x0311` | `NETWORK_URL` / `NETWORK_HTTP_OTA_LONG` | — |
| 6 | 791 | `0x0317` | `NETWORK_REGION` / `NETWORK_HTTP_OTA` | — (sets `toniebox_firmware.rtnlRegion`) |
| 6 | 806 | `0x0326` | `NETWORK_HTTP_FIRMWARE_PATH` | — |
| 6 | 8498 | `0x2132` | `NETWORK_DOWNLOAD_PROGRESS` | — |
| 6 | 1009 | `0x03F1` | `NETWORK_DOMAIN` | — (sets `internal.rtnl.prodDomain`) |
| 8 | 703 | `0x02BF` | `FIRMWARE_FULL_VERSION` | — |
| 8 | 704 | `0x02C0` | `FIRMWARE_VERSION` | — |
| 8 | 7146 | `0x1BEA` | `FIRMWARE_INFOS` | — |
| 12 | 15426 | `0x3C42` | `TILT_A_ESP32` | **`BoxTilt-A`** |
| 12 | 15427 | `0x3C43` | `TILT_B_ESP32` | **`BoxTilt-B`** |
| 15 | 8627 | `0x21B3` | `TAG_VALID_CC3200` | **`TagValid`** |
| 15 | 8630 | `0x21B6` | `TAG_VALID_REMOVED_CC3200` | **none** (MQTT only) |
| 15 | 8646 | `0x21C6` | `TAG_INVALID_CC3200` | **`TagInvalid`** |
| 15 | 15452 | `0x3C5C` | `TAG_INVALID_ESP32` | **`TagInvalid`** |
| 15 | 16065 | `0x3EC1` | `TAG_VALID_ESP32` | **`TagValid`** |
| 15 | 16067 | `0x3EC3` | `TAG_VALID_REMOVED_ESP32` | **none** (MQTT only) |
| 20 | 6803 | `0x1A93` | `AUDIO_ID_CC3200` | **`ContentAudioId` + `ContentTitle`** (+ `ContentPicture`) |
| 20 | 16096 | `0x3EE0` | `AUDIO_ID_ESP32` | **`ContentAudioId` + `ContentTitle`** (+ `ContentPicture`) |
| 22 | 297 | `0x0129` | `AUDIO_ID` | **`ContentAudioId` + `ContentTitle`** (+ `ContentPicture`) |
| 22 | 6212 | `0x1844` | `AUDIO_1` | — |
| 22 | 6480 | `0x1950` | `AUDIO_PLAY` | — (system-sound paths ⇒ `playback:stopped`) |
| 27 | 8672 | `0x21E0` | `VOLUME_CHANGE_CC3200` | **`VolumeLevel` + `VolumedB`** |
| 27 | 15524 | `0x3CB4` | `VOLUME_CHANGE_ESP32` | **`VolumeLevel` + `VolumedB`** |
| 37 | 1009 | `0x03F1` | `NETWORK_DOMAIN` | — (sets `internal.rtnl.rtnlDomain`) |

Note `15-6803` and `20-6803` share a function value; the **group** disambiguates. The legacy UI
keys on the string `"{group}-{function}"` (`legacy.html:1649-1696`) — the right model.

### 7.3 `log3.field2` → derived event — `include/handler_rtnl.h:87-100`

```c
87: typedef enum
88: {
89:     RTNL3_TYPE_EAR_BIG = 1,
90:     RTNL3_TYPE_EAR_SMALL = 2,
91:     RTNL3_TYPE_KNOCK_FORWARD = 3,
92:     RTNL3_TYPE_KNOCK_BACKWARD = 4,
93:     RTNL3_TYPE_TILT_FORWARD = 5,
94:     RTNL3_TYPE_TILT_BACKWARD = 6,
95:     RTNL3_TYPE_CHARGER_ON = 7,
96:     RTNL3_TYPE_CHARGER_OFF = 8,
97:     RTNL3_TYPE_PLAYBACK_STARTING = 11,
98:     RTNL3_TYPE_PLAYBACK_STARTED = 12,
99:     RTNL3_TYPE_PLAYBACK_STOPPED = 13,
100: } rtnl_log3_type;
```

| Value | Constant | Derived SSE | MQTT entity |
|---|---|---|---|
| 1 | `EAR_BIG` | `pressed: ear-big` \| `ear-big-double` | `VolUp` |
| 2 | `EAR_SMALL` | `pressed: ear-small` \| `ear-small-double` | `VolDown` |
| 3 | `KNOCK_FORWARD` | `knock: forward` | — |
| 4 | `KNOCK_BACKWARD` | `knock: backward` | — |
| 5 | `TILT_FORWARD` | `tilt: forward` | — |
| 6 | `TILT_BACKWARD` | `tilt: backward` | — |
| 7 | `CHARGER_ON` | `charger: on` | `Charger` `ON` |
| 8 | `CHARGER_OFF` | `charger: off` | `Charger` `OFF` |
| 11 | `PLAYBACK_STARTING` | `playback: starting` **+ `playback: started`** | `Playback` `OFF` then `ON` |
| 12 | `PLAYBACK_STARTED` | `playback: started` | `Playback` `ON` |
| 13 | `PLAYBACK_STOPPED` | `playback: stopped` | `Playback` `OFF` |
| other | — | none, logs `Not-yet-known log3 type` | — |

### 7.4 Box → server HTTP route

```c
// src/server.c:106
{REQ_ANY, "*binary", SERTY_BOTH, &handleRtnl},
```

The request is not a well-formed HTTP request; binary detection happens in
`src/cyclone/cyclone_tcp/http/http_server.c:475-494`. The body is a sequence of **big-endian 4-byte
length-prefixed protobuf messages** (`handler_rtnl.c:102`); one logical message may span several
TCP/TLS records (`:143-145`).

---

## 8. Radio / stream tonies on the SSE stream

### 8.1 How a radio tonie is configured

`content.json` in the content directory. The decisive classification is in
`src/contentJson.c:97-135`:

```c
97: if (osStrlen(content_json->source) > 0)
98: {
99:     resolveSpecialPathPrefix(&content_json->_source_resolved, settings);
100:    if (isValidTaf(content_json->_source_resolved, settings->core.full_taf_validation))
101:    {
102:        content_json->_source_type = CT_SOURCE_TAF;
103:    }
104:    else
105:    {
...
118:        else if (fsFileExists(content_json->_source_resolved) || osStrstr(content_json->_source_resolved, "://"))
119:        {
120:            content_json->_source_type = CT_SOURCE_STREAM;
121:            if (!content_json->live || !content_json->nocloud)
122:            {
123:                content_json->live = true;
124:                content_json->nocloud = true;
125:                content_json->_updated = true;
126:            }
127:        }
```

**`osStrstr(source, "://")` is the whole radio feature.** A `source` containing `://` becomes
`CT_SOURCE_STREAM` and teddyCloud force-sets `live=true, nocloud=true` for you.
`resolveSpecialPathPrefix()` (`src/server_helpers.c:835-860`) only rewrites `data://`,
`content://`, `lib://` etc. — URLs pass through untouched to ffmpeg.

`ct_source_t` — `include/contentJson.h:10-18`:

```c
typedef enum
{
    CT_SOURCE_NONE,
    CT_SOURCE_TAF,
    CT_SOURCE_TAF_INCOMPLETE,
    CT_SOURCE_TAP_STREAM,
    CT_SOURCE_TAP_CACHED,
    CT_SOURCE_STREAM,
} ct_source_t;
```

The URL is handed **raw to ffmpeg** (`src/toniefile.c:586-617`):

```c
snprintf(ffmpeg_command, sizeof(ffmpeg_command), "ffmpeg -i \"%s\" -f s16le -acodec pcm_s16le -ar 48000 -ac 2 -ss %" PRIuSIZE " -", input_source, skip_seconds);
...
FILE *ffmpeg_pipe = osPopen(ffmpeg_command, "r");
```

⇒ ffmpeg's own protocol support (http, https, rtmp, icy) applies. **No teddyCloud-side proxy,
authentication, headers, retries, or caching.**

### 8.2 The critical asymmetry: serving a stream emits ZERO SSE

```
$ rg -n "sse_" src/handler_cloud.c src/handler.c src/handler_api.c
(no matches)
```

**`src/handler_cloud.c` emits no SSE events at all.** The freshness check, the content request, the
ffmpeg start, the stream teardown — all invisible on SSE. The only MQTT side-effects are
`LastCloudFreshnessCheckTime` (`:1046`, `:1124`), `LastCloudContentTime` (`:474`), and
`cmd:stop` when `CmdStop` is used.

### 8.3 Placement in radio mode — same events, different values

**Answer: the same SSE events.** There is **no source-type branch anywhere in the SSE emission
path** — the content-type dispatch is confined to `src/handler_cloud.c:638/689/737`,
`src/handler.c:873-882`, `src/handler_api.c:1711`. `TagValid` comes from `handler_rtnl.c:342-345`
and `playback:*` from `:318-324`, neither of which consults content type.

| Step | What happens | SSE |
|---|---|---|
| 0 | `POST /v1/freshness-check` — box gets content metadata | **none** |
| 1 | `POST /*binary` log2 15-16065 | `rtnl-raw-log2`, **`TagValid`** |
| 2 | `GET /v2/content/<ruid>` — ffmpeg starts, TAF streams | **none** |
| 3 | `POST /*binary` log3 field2 = 11 | `rtnl-raw-log3`, **`playback:starting`**, **`playback:started`** |
| 4 | `POST /*binary` log2 20-16096 / 22-297 *(if the box sends it)* | `rtnl-raw-log2`, **`ContentAudioId`**, **`ContentTitle`**, maybe **`ContentPicture`** |
| 5 | `POST /*binary` log2 22-6480 | `rtnl-raw-log2` only (`tbs_playback_file` ignores non-system-sound paths) |

#### The audio id is a radio fingerprint

```c
// src/toniefile.c:740-745
    int32_t size = 0;
    if (isStream)
    {
        size = get_settings()->encode.stream_max_size - TONIE_HEADER_LENGTH;
    }
    toniefile_t *taf = toniefile_create(target_taf, time(NULL) - TEDDY_BENCH_AUDIO_ID_DEDUCT, append, size);
```

`#define TEDDY_BENCH_AUDIO_ID_DEDUCT 0x50000000` (`include/toniefile.h:33`).

⇒ **`ContentAudioId` for a radio tonie = `time(NULL) - 0x50000000`, in decimal, and it is a fresh
value on every connection.** Two consequences:

1. **It is always `< 0x50000000`** (1342177280), while every genuine Tonie audio id is larger. This
   is a **reliable radio discriminator** — the same threshold teddyCloud itself uses at
   `src/handler_rtnl.c:374` and `src/handler_cloud.c:904-908`:
   ```c
   /* custom tonies from teddyBench have the audio id reduced by a constant */
   if (unix_time < TEDDY_BENCH_AUDIO_ID_DEDUCT)
   {
       unix_time += TEDDY_BENCH_AUDIO_ID_DEDUCT;
       *isCustom = true;
   }
   ```
2. **`ContentAudioId + 0x50000000` = the Unix time at which ffmpeg started for that stream**, to
   1-second resolution. You can compute stream age and detect restarts.

It is also why a stream can **never** match `tonies.json`: `src/toniesJson.c:453` would need an
entry listing exactly `audioId + 0x50000000`, i.e. one specific wall-clock second. The same
trick is used for `CT_SOURCE_TAP_STREAM` at `src/handler_cloud.c:699`.

#### `ContentTitle` for a stream

`tonies_byAudioId(448376320)` searches `tonies.custom.json` then `tonies.json` and can never match
⇒ `item == NULL` ⇒ fall through to the **model fallback** (`handler_rtnl.c:360-368`):

```c
tonie_info_t *tonieInfo = getTonieInfoFromUid(client_ctx->state->tag.uid, false, client_ctx->settings);
if (tonieInfo->valid || (tonieInfo->json._valid && tonieInfo->json.tonie_model != NULL))
{
    item = tonies_byModel(tonieInfo->json.tonie_model);
}
```

1. **`tonie_model` set in `content.json` and resolvable** ⇒ `item != NULL` ⇒ emits
   **`ContentTitle` = the model's title** and **`ContentPicture` = the raw `item->picture`**.
2. **`tonie_model` absent or unknown** ⇒ `item == NULL` ⇒ emits **`ContentTitle` = `"Unknown"`** and
   **no `ContentPicture` on SSE at all** (the `img_custom.png` fallback at `:377` is MQTT-only).
   Because `448376320 < 0x50000000`, MQTT gets `img_custom.png`, not `img_unknown.png`.

**⇒ `ContentPicture` on SSE fires ONLY when the title lookup succeeded. There is no "unknown"
picture on SSE.** The `"Unknown"`-with-no-picture combination is the *normal* case for a radio
tonie without a resolvable `tonie_model`.

The model fallback depends on `state->tag.uid`, which is **zeroed by `playback:stopped`**
(`toniebox_state.c:87`) — so after any stop, the first content report will say `"Unknown"` until
the next `TagValid`.

#### `CT_SOURCE_STREAM` vs `CT_SOURCE_TAP_STREAM` — zero SSE difference

Both branches (`src/handler_cloud.c:638-688` and `:689-736`) use the same `stream_ctx`, both set
`stop_on_playback_stop = true`, both write `time(NULL)-0x50000000` as the audio id, both die on
log3-13, and neither emits a single SSE byte. Differences are entirely server-side:

| | `CT_SOURCE_STREAM` | `CT_SOURCE_TAP_STREAM` |
|---|---|---|
| Producer | live ffmpeg pipe, unbounded | `tap_generate_taf()` renders the whole playlist up front (`src/tonie_audio_playlist.c:179-216`) |
| Target | `<content>.stream`, grows while playing | `<tapfile>.tap.tmp` then moved into place |
| `isStream` | `true` (`toniefile.c:875`) | `false` (`tonie_audio_playlist.c:202`) |
| TAF `num_bytes` | `stream_max_size - 4096` (fabricated) | real `audio_length` |
| Ends on its own | never | yes, when the playlist is exhausted |
| **SSE** | **none** | **none** |

### 8.4 Does the box send an audio-id report for a live stream? [E] — unknown

**The code cannot answer this.** `src/handler_rtnl.c:351` is completely unconditional — it does not
consult `tonieInfo`, `content.json`, `_source_type`, or anything else. teddyCloud is a purely passive
receiver. Whether the box *chooses* to send 20-6803/16096 or 22-297 for a stream is a Toniebox
firmware behaviour, and there is **no evidence in this repository either way**:
`docs/api/custom-tonies.md` says nothing; the captured samples in comments
(`handler_rtnl.c:454, 463, 469, 478`) are firmware-info, region, prod domain and rtnl domain —
none audio-id, none stream-related; `tests/`, `dev-sandbox/`, `fake-server/` contain no captures.

**Indirect evidence that it probably does** [P]: the box reports a real per-tag `audio_id` in every
freshness check, and teddyCloud has dedicated handling for the "reduced by the teddyCloud constant"
case:

```c
// src/handler_cloud.c:954-978
uint32_t boxAudioId = freshReq->tonie_infos[i]->audio_id;
...
checkAudioIdForCustom(&custom_box, date_buffer_box, boxAudioId);
if (custom_box)
    boxAudioId += TEDDY_BENCH_AUDIO_ID_DEDUCT;
```

So the box receives, parses and stores the audio id of a streamed TAF. But "likely" ≠ "proven".

**To settle it empirically:** set `rtnl.logRaw=true` / `rtnl.logHuman=true`, clear the logs, place a
radio tag, then look for `function_group=20, function=6803|16096` or `22/297` in the
`POST /*binary` stream, and cross-check the 4 payload bytes against `time(NULL) - 0x50000000`.
The server-side decode is a `TRACE_DEBUG` in `rtnlEventLog()` (`handler_rtnl.c:495-530`), so
`log.debug=true` suffices.

### 8.5 What is the observable signal that a radio is playing?

| Candidate | Reliable for radio? | Why |
|---|---|---|
| `TagValid` | **YES — guaranteed** | `handler_rtnl.c:342-345`, unconditional, source-type-agnostic. Fires before any content request. |
| `playback:starting` + `playback:started` | **YES — guaranteed** | `handler_rtnl.c:318-322` → `toniebox_state.c:73-79`, unconditional. |
| `ContentAudioId` | **PROBABLY [E]** | Code is source-agnostic so *if* the box sends it, it is emitted. When present it is **strictly the best signal** because its **value** identifies a stream (`< 0x50000000`) and gives the ffmpeg start time. |
| `ContentTitle` | follows `ContentAudioId` | `"Unknown"` is the normal radio result. **Not** a stream indicator — real cached custom tonies hit the same code. |
| `ContentPicture` | **conditional** | only when the title lookup succeeded; never for a bare radio tonie. |
| log2 `22-6480` | **invisible on SSE** | only via `rtnl-raw-log2` |
| `rtnl-raw-log2` / `-log3` | **YES — 100% guaranteed** | emitted at the very top of `rtnlEvent()` (`:209`, `:253`) before any filtering, with zero dependence on content type, state, or ffmpeg. **This is the guaranteed-observable channel.** |

**Definitive answer [P]:** the reliable "the radio is playing" signal is the pair
`playback:starting` → `playback:started`, qualified by a preceding `TagValid`. `ContentAudioId` is
the best signal *if and only if* the box emits the audio-id report for streams ([E] §8.4), and when
present it dominates.

### 8.6 Lifting the tonie — does the stream stop?

**Yes, via two independent mechanisms.** The second is a guaranteed backstop.

#### `tbs_tag_removed()` — verbatim

```c
// src/toniebox_state.c:42-56
void tbs_tag_removed(client_ctx_t *client_ctx, uint64_t uid, bool valid)
{
    client_ctx->state->tag.uid = uid;
    client_ctx->state->tag.valid = valid;
    if (valid)
    {
        setLastUid(client_ctx->state->tag.uid, client_ctx->settingsNoOverlay);
    }

    char cuid[16 + 2];
    osSprintf((char *)cuid, "-%016" PRIX64 "", (int64_t)uid);

    mqtt_sendBoxEvent(valid ? "TagValid" : "TagInvalid", cuid, client_ctx);
    mqtt_sendBoxEvent(!valid ? "TagValid" : "TagInvalid", "", client_ctx);
}
```

The caller hard-codes `valid = true` (`handler_rtnl.c:348`), so after removal
`tag.uid = <removed uid>` and **`tag.valid = TRUE`**. **No `sse_sendEvent` call exists.**

#### Every `stream_ctx.active` assignment site

| # | Site | Statement | Trigger | On tag lift? |
|---|---|---|---|---|
| 1 | `src/toniefile.c:757` | `*active = true;` | ffmpeg task starts producing | — |
| 2 | `src/toniefile.c:816` | `*active = false;` | encode loop ended (EOF/error) | no |
| 3 | `src/toniebox_state.c:83` | `stream_ctx.active = false;` | **`TBS_PLAYBACK_STOPPED`** with `stop_on_playback_stop && !quit && tag.valid` | **YES (fast path)** |
| 4 | `src/toniebox_state.c:151` | `stream_ctx.active = false;` | **`tbs_cmd_stop()`** (MQTT `CmdStop`) | on command |
| 5 | `src/handler_cloud.c:652` | `stream_ctx->active = false;` | init before ffmpeg (STREAM) | — |
| 6 | `src/handler_cloud.c:683` | `stream_ctx->active = false;` | **after `httpSendResponseStream()` returns** | **YES (backstop)** |
| 7 | `src/handler_cloud.c:703` | `stream_ctx->active = false;` | init (TAP) | — |
| 8 | `src/handler_cloud.c:730` | `stream_ctx->active = false;` | after the send returns (TAP) | **YES (backstop)** |

`quit` is only set `true` by the ffmpeg/tap task itself finishing (`toniefile.c:869`, `:877`;
`tonie_audio_playlist.c:224`). `stop_on_playback_stop` is set `true` at `handler_cloud.c:655`
(STREAM) and `:706` (TAP) and is **never reset to false** — once any stream has been served it
stays `true` for the process lifetime.

#### Mechanism 1 — log3 `field2=13` (fast path, ~100 ms)

```c
// src/toniebox_state.c:80-88
case TBS_PLAYBACK_STOPPED:
    if (client_ctx->state->box.stream_ctx.stop_on_playback_stop && !client_ctx->state->box.stream_ctx.quit && client_ctx->state->tag.valid)
    {
        client_ctx->state->box.stream_ctx.active = false;
    }
```

All three conditions hold on tag lift:
- `stop_on_playback_stop` → `true` (`handler_cloud.c:655`) ✔
- `!quit` → ffmpeg is still running ✔
- `tag.valid` → **`true`**, because `tbs_tag_removed` sets it true (`:45`) ✔ — and it is also true
  if the log3-13 arrives *before* the 15-8630 removal report, because placement left it true. Either
  ordering works.

⇒ SSE: `rtnl-raw-log3`, **`playback:stopped`**.

Git history names the failure mode this targets:

```
commit 2671b0000e68f4f0143f830680230147e819e81c
Author: SciLor
Date:   Tue Apr 2 19:02:11 2024 +0000

    Enhance box state for better architecture
    stop webradio on system message whichs stops playback (connection loss/buffer underrun)
```

Also reachable via log2 `22-6480` with a system-sound path — the "connection loss / buffer
underrun" case.

#### Mechanism 2 — HTTP transport teardown (the backstop, always fires)

```c
// src/handler_cloud.c:665-688
if (stream_ctx->error == NO_ERROR)
{
    if (client_ctx->settings->encode.ffmpeg_sweep_startup_buffer)
    {
        osDelayTask(client_ctx->settings->encode.ffmpeg_sweep_delay_ms);
    }

    uint32_t delay = client_ctx->settings->encode.ffmpeg_stream_buffer_ms;
    TRACE_INFO("Serve streaming content from %s, delay %" PRIu32 "ms\r\n", tonieInfo->json.source, delay);
    ffmpeg_ctx.sweep = false;

    osDelayTask(delay);
    error_t response_error = httpSendResponseStream(connection, streamFileRel, true);
    if (response_error) { TRACE_ERROR(...); }
}
stream_ctx->active = false;
while (!stream_ctx->quit)
{
    osDelayTask(100);
}
```

The send loop (`src/cyclone/cyclone_tcp/http/http_server.c:1167-1217`):

```c
   while (length > 0)
   {
      n = MIN(length, HTTP_SERVER_BUFFER_SIZE);
      error = fsReadFile(file, connection->buffer, n, &n);
      if (isStream && error == ERROR_END_OF_FILE && connection->private.client_ctx.state->box.stream_ctx.active)
      {
         osDelayTask(100);
         error = httpCloseStream(connection);
         if (error) break;
         continue;
      }
      if (error) break;

      error = httpWriteStream(connection, connection->buffer, n);
      if (error) break;
      length -= n;
   }
```

The socket is **non-blocking** (`src/platform/platform_linux.c:205-245`): `send()` returning
`EAGAIN`/`EWOULDBLOCK` maps to `ERROR_TIMEOUT`, anything else to `ERROR_WRITE_FAILED`. The loop can
only exit four ways, and **three of them end the stream**:

1. `length` reaches 0 (all `stream_max_size` delivered) → return → `active = false`.
2. `fsReadFile` hits EOF **and `stream_ctx.active` is already false** → `break` → `active = false`.
3. `httpWriteStream` fails — RST/FIN from the box, or `EAGAIN` because the box stopped reading so
   the send buffer + receive window filled → `break` → `active = false`.
4. `httpCloseStream` fails inside the EOF-wait branch → `break` → `active = false`.

⇒ **There is no path where the box stops consuming but teddyCloud keeps ffmpeg alive
indefinitely.** When the tag is lifted and the box stops pulling bytes, the buffers fill within
milliseconds/seconds, `send()` returns `EAGAIN`, and the stream is torn down.

#### The teardown chain

```c
// src/toniefile.c:757-758 (loop exit)
    while (*active) { ... encode ... }

// src/toniefile.c:809-818
    if (!(*active)) { TRACE_INFO("Encoding aborted, active flag set to false\r\n"); }
    else            { *active = false; }

// src/toniefile.c:875-877
    stream_ctx->error = ffmpeg_stream(source, 1, ..., &stream_ctx->active, &ffmpeg_ctx->sweep, ffmpeg_ctx->append, true);
    stream_ctx->quit = true;
```

`active=false` → the encode loop exits within one 60 ms chunk → `toniefile_close()` rewrites the
TAF header with the **real** `num_bytes` and SHA1 (`toniefile.c:354-378`) → `quit=true` → the wait
loop at `handler_cloud.c:684` exits → the request handler returns and the server closes the socket.

Because the box was promised `Content-Length: 251658239` and got a fraction, it observes a
**truncated download** and will treat the file as incomplete.

#### Residual cases where audio may continue [E]

| Case | Assessment |
|---|---|
| Box keeps the socket open and slowly reads after the tag is lifted | Would run until `stream_max_size` (~6 h) per the `encode.stream_max_size` description: *"the box will stop after the file is full and you'll need to replace the tag onto the box."* Requires the box to keep reading at real-time rate while not playing — contradicts "lifted". **Cannot be proven from code.** |
| **Box keeps playing its own already-buffered audio** | **Real and documented:** `MQTT_CONTROL.md:20` — *"CmdStop stops server-side stream delivery; the box may continue playing already-buffered audio."* Buffer depth is bounded by `encode.ffmpeg_stream_buffer_ms` (default 2000 ms, `settings.c:287`) plus whatever it downloaded ahead into flash. teddyCloud cannot reach it — there is no command channel. |
| Network drop | RST/FIN → `ERROR_WRITE_FAILED` → immediate teardown. Silent vanish (power-off) → buffer fills → `ERROR_TIMEOUT` → teardown. **Self-healing** given non-blocking `send()`. |
| teddyCloud restarts mid-stream | `state->tag.valid` is lost, so the log3-13 guard at `:81` fails and the stream is **not** killed by the semantic path — only by the transport backstop (the old connection dies with the process). |
| Two concurrent content requests for one box | `handler_cloud.c:652` unconditionally zeroes `active` and starts a **second** ffmpeg task sharing the same `stream_ctx`, with no mutex. Stopping one kills the other. Not expected in practice (the box opens one download at a time). |

**Verdict: teddyCloud does not keep a radio stream running for 6 hours after the tag is lifted.**
Both the semantic and the transport path kill ffmpeg. The only residual is the box's own buffered
audio, which teddyCloud cannot touch.

### 8.7 Stream lifetime and re-placement

**The `.stream` file is never deleted by teddyCloud.** The only deletion path
(`fsDeleteFile(target_taf)`, `toniefile.c:849`) is skipped for streams:

```c
// src/toniefile.c:842-850
    else if (isStream)
    {
        TRACE_INFO("TAF encoding stopped (streaming)\r\n");
    }
    else
    {
        TRACE_ERROR("TAF encoding failed, deleting TAF\r\n");
        fsDeleteFile(target_taf);
    }
```

⇒ streams are **never** deleted even on ffmpeg/ffprobe failure. Remove them explicitly via
`/api/fileDelete`.

**Overwrite vs append is decided by the box's HTTP Range header:**

```c
// src/handler_cloud.c:645-646
ffmpeg_ctx.append = (connection->request.Range.start != 0);
ffmpeg_ctx.sweep = client_ctx->settings->encode.ffmpeg_sweep_startup_buffer;
```

```c
// src/toniefile.c:119-140 (toniefile_create)
if (!fsFileExists(fullPath)) { append = false; }
if (append)
{
    ctx->file = fsOpenFileEx(fullPath, "r+");
    TRACE_INFO("Append to TAF: %s\n", fullPath);
    fsSeekFile(ctx->file, 4, SEEK_SET);
    fsReadFile(ctx->file, buffer, TONIEFILE_FRAME_SIZE - 4, &read_length);
    tafHeader = toniebox_audio_file_header__unpack(NULL, read_length, (uint8_t *)buffer);
    audio_id = tafHeader->audio_id;
    ctx->taf.audio_id = audio_id;
}
else
{
    ctx->file = fsOpenFile(fullPath, FS_FILE_MODE_WRITE | FS_FILE_MODE_CREATE | FS_FILE_MODE_TRUNC);
}
```

| Request | `Range.start` | `append` | Effect on `.stream` |
|---|---|---|---|
| Fresh placement | `0` | `false` | **TRUNCATED**, new `audio_id = time(NULL)-0x50000000` |
| Box resuming a partial download | `!= 0` | `true` | **kept**, `audio_id` **read back from the existing header** (unchanged), new audio appended |

⇒ **A new request with `Range.start == 0` always destroys the old buffer**, even for the same tag.

**`encode.ffmpeg_stream_restart` — the entire effect is in the `Content-Range` total:**

```c
// src/cyclone/cyclone_tcp/http/http_server.c:1080-1088
   file_length = length;                  // real size from fsGetFileSize()
   if (isStream)
   {
      length = connection->private.client_ctx.settings->encode.stream_max_size;
      if (!connection->private.client_ctx.settings->encode.ffmpeg_stream_restart)
      {
         file_length = length;
      }
   }
```

| `ffmpeg_stream_restart` | Box is told | Effect |
|---|---|---|
| `false` (**default**) | `Content-Range: bytes S-E/251658239` | offsets stable over the whole 6 h ⇒ the box **resumes where it left off** and audibly re-plays already-downloaded radio |
| `true` | total = the *current* size of the freshly created `.stream` (only the ~4 s of prebuffer) | box concludes it is a different file ⇒ **restart from the top**, slower, but no duplicated audio and previous data deleted on the box |

Setting definition — `src/settings.c:288`:

> If a stream is continued by the box, a new file is forced. This has the cost of a slower restart,
> but does not play the old buffered content and deletes the previous stream data on the box.

**`Content-Length` and EOF.** The box is always promised
`Content-Length: 251658239` (`encode.stream_max_size`, default `1024*1024*40*6-1`, `settings.c:291`).
The TAF header's `num_bytes` is set to `stream_max_size - 4096` while streaming
(`toniefile.c:743`), so header and HTTP agree. When ffmpeg stops, `toniefile_close()` rewrites the
header with the **true** `num_bytes` and SHA1 — so the on-disk file is always correct; only the
in-flight HTTP `Content-Length` was a promise. The send loop then stalls 100 ms per iteration at
EOF until `active` goes false, and the response ends **short**.

### 8.8 The "no events at all" hypothesis — tested

**The hypothesis is partly right, but the conclusion is wrong.**

#### What the code *guarantees* fires, regardless of source type

The log3 dispatch is completely independent of source type:

```c
// src/handler_rtnl.c:262-330 (structure)
if (rpc->log3)
{
    settings_internal_rtnl_t *rtnl_setting = &client_ctx->settings->internal.rtnl;
    switch (rpc->log3->field2)
    {
    ...
    case RTNL3_TYPE_PLAYBACK_STARTING: tbs_playback(client_ctx, TBS_PLAYBACK_STARTING); break;
    case RTNL3_TYPE_PLAYBACK_STARTED:  tbs_playback(client_ctx, TBS_PLAYBACK_STARTED);  break;
    case RTNL3_TYPE_PLAYBACK_STOPPED:  tbs_playback(client_ctx, TBS_PLAYBACK_STOPPED);  break;
    default: TRACE_WARNING("Not-yet-known log3 type: %u\r\n", rpc->log3->field2); break;
    }
}
```

`rpc->log3->field2` is read straight off the wire; `tbs_playback` never looks at tonie info,
`content.json`, or ffmpeg. And before any semantic filter:

```c
// src/handler_rtnl.c:207-260
if (rpc->log2) { sse_startEventRaw("rtnl-raw-log2"); ...; sse_endEventRaw(); }
if (rpc->log3) { sse_startEventRaw("rtnl-raw-log3"); ...; sse_endEventRaw(); }
```

#### The realistic near-silent scenario

A radio tonie placed where the box **never** sends an audio-id report would produce exactly:

```
event: rtnl-raw-log2   data: {..., "function_group": 15, "function": 16065, ...}
event: TagValid         data: "...1122334455667788"
event: rtnl-raw-log3   data: {..., "field2": 11}
event: rtnl-raw-log3   data: {..., "field2": 12}
event: playback         data: "starting"
event: playback         data: "started"
event: rtnl-raw-log3   data: {..., "field2": 13}
event: playback         data: "stopped"
```

— with **no** `ContentAudioId`, **no** `ContentTitle`, **no** `ContentPicture`, and **no event
whatsoever indicating that a stream is being served**. Four semantic SSE events for the entire
radio session, none of which distinguishes a radio from a cached TAF.

#### Minimum-guaranteed vs. maybe-absent

**MINIMUM GUARANTEED for a radio tonie [P]:**

| Event | Source | Guarantee |
|---|---|---|
| `rtnl-raw-log2` | `handler_rtnl.c:209` | for every RTNL message, no conditions |
| `rtnl-raw-log3` | `handler_rtnl.c:253` | for every RTNL message with a log3 |
| `TagValid` | `:344` → `toniebox_state.c:36` | on 15-8627 / 15-16065 |
| `playback:starting` + `playback:started` | `:318,321` → `toniebox_state.c:74,77` | on log3 field2 11 and/or 12 (11 emits both) |
| `playback:stopped` | `:324` → `toniebox_state.c:89` | on log3 field2 13, incl. the 22-6480 system-sound variant |
| `keep-alive` | `handler_sse.c:83` | every ~16 s — liveness only |

**MAY BE ABSENT:**

| Event | Why |
|---|---|
| `ContentAudioId` | only if the box omits log2 20-6803/16096 or 22/297 for streams — **[E]** §8.4 |
| `ContentTitle` | only inside the audio-id branch — **structurally impossible** without `ContentAudioId` |
| `ContentPicture` | only at `:395`, i.e. only if the title lookup succeeded; never a fallback |
| any "this is a stream" indicator | **does not exist on SSE** — `handler_cloud.c` emits zero SSE |
| any tag-removal indicator | **does not exist on SSE** — `tbs_tag_removed` has no `sse_sendEvent` |
| any "stream started / stopped" indicator | **does not exist** — only MQTT `…/LastCloudContentTime` and the `cmd:stop` SSE frame |

### 8.9 Controller design for radio mode

#### State machine

```
IDLE
  --TagValid(uid)-->        TAGGED(uid)               [uid for ruid conversion]
  --playback:starting-->    PENDING(uid)
  --playback:started-->     PLAYING(uid)
  --ContentAudioId a-->     PLAYING_RADIO(uid, a)     if a < 0x50000000
                            PLAYING_TAF(uid, a)        otherwise
  --playback:stopped-->     IDLE
```

Always parse `rtnl-raw-log2` / `rtnl-raw-log3` in parallel — they are the only channel that cannot
be silently withheld, they give you the `15-8630/16067` tag-removal signal, and they let you settle
the audio-id question from a live feed.

#### uid → ruid

```
uid  = "1122334455667788"
ruid = "88776655" + "44556677"   ->  <contentdir>/88776655/44556677
```

(each 8-char half byte-reversed, `src/handler.c:647-658`)

#### Detecting "tag lifted" — there is no SSE event. Options ranked

| Option | Viability | Detail |
|---|---|---|
| **A. `playback:stopped`** | **Best** | Unconditional from `toniebox_state.c:89`. It is also the signal teddyCloud itself uses to kill the stream. Limitation: a *playback* event — the box also sends it for buffer underrun and playback-finished. Exactly right for "is the radio still going". |
| **B. `rtnl-raw-log2` grp 15 fn 16067 / 8630** | **Best *tag* signal**; needs raw parsing | Routed to `tbs_tag_removed` (no SSE), but the enclosing `rtnl-raw-log2` frame is emitted unconditionally and carries `function_group`/`function` as top-level JSON keys. The only lossless tag-removal signal on SSE. |
| **C. MQTT `…/TagValid` with `-` prefix** | works, needs MQTT | `toniebox_state.c:52` — the leading minus is the marker. Topic `{mqtt.topic}/box/{box_cn}/TagValid`. |
| **D. poll `/api/getTagInfo`** | **does NOT work** | `getTagInfoJson` (`handler_api.c:4340-4458`) reports *content* facts (`valid`, `exists`, `live`, `nocloud`, `source`) — there is **no "on the box right now" field**. |
| **E. poll `internal.last_ruid`** | **does NOT work** | `setLastUid` is called from `tbs_tag_placed`, `tbs_tag_removed` **and** `handleCloudClaim` — and `tbs_tag_removed` calls it with `valid==true`, so the value is **rewritten to the removed tag's ruid** on removal. It is "last ruid seen", not "currently placed". **No endpoint in v0.7.0 exposes `state->tag.valid`/`tag.uid`.** |
| **F. `/api/getBoxes`** | no | returns only `ID`, `commonName`, `boxName`, `boxModel` |

**Recommended:** combine **A** + **B**. Add a watchdog only if you also control the box's download
(you don't — SSE is one-way).

**Corollary:** teddyCloud's tag state is lost across restarts, so if teddyCloud restarts
mid-playback the log3-13 guard at `:81` fails and only the transport backstop kills the stream.
Another reason not to rely on option A alone.

#### Stopping the stream

**MQTT `CmdStop` is the only stop mechanism that exists** — see §9.3. It is server-side only: the
box may continue playing buffered audio.

#### Detecting natural end of stream

There is **no SSE event that says the 240 MB is exhausted**. The box hits its own buffer underrun
and emits `playback:stopped` (log3 13). Additionally, a **new** `ContentAudioId` on the next stream
attempt has a **higher** value. If you need an absolute wall-clock limit, track
`ContentAudioId + 0x50000000` and compare against `encode.stream_max_size` / 6 h yourself.

---

## 9. Control channel — how to actually drive the box

### 9.1 Architecture

```
┌──────────────┐  1. GET /api/sse  (SSE, server→client only)
│  Your app    │◀──────────────────────────────────────────┐
└──────┬───────┘                                           │
       │ 2. MQTT publish  {mqtt.topic}/box/{cn}/Cmd*/set    │
       ▼                                                   │
┌──────────────┐                                           │
│ teddyCloud   │                                           │
│  mqtt.c ─────┼──▶ mqttTestPublishCallback (mqtt.c:200)    │
│              │        └─ ha_received() (home_assistant.c:203)
│              │             └─ mqtt_box_cmd_*_rx (mqtt.c:826-945)
│              │                  └─ tbs_cmd_*() (toniebox_state.c:141-242)
│              │                       ├─ CmdStop : state->box.stream_ctx.active = false
│              │                       └─ others  : settings_set_*_id(...) → config/<cn>.conf
│              │  3. ffmpeg -i "http://…radio…"  (toniefile.c:586-617)
│              │  4. next POST /v1/freshness-check → setTonieboxSettings() (handler.c:660-672)
│              ▼                                                    ▼
┌──────────────┐                                        ┌────────────────┐
│  Toniebox    │◀── GET /v1/content/<ruid> (Range) ────│  Box applies   │
│  (TB1/TB2)   │    200, Content-Length = 240 MiB       │  on that check │
└──────────────┘    (fixed, non-chunked, held open)     └────────────────┘
       └──▶ POST *binary (RTNL) ──▶ handler_rtnl.c ──▶ sse_sendEvent / mqtt_sendBoxEvent
```

**Two independent MQTT code paths — do not confuse them:**

| | `src/mqtt.c` | `src/mqtt_server.c` |
|---|---|---|
| Role | MQTT **client** → your broker | MQTT **server** → Toniebox connects to it |
| API | `mqtt_init/sendEvent/sendBoxEvent/publish/subscribe` | `mqtt_server_init/task/publish_fresh_tonies` |
| Purpose | expose state/commands to HA + your app | serve the box its own MQTT protocol |

### 9.2 Enabling MQTT

| Setting | Default | Notes |
|---|---|---|
| `mqtt.enabled` | `FALSE` | master switch |
| `mqtt.hostname` | `""` | broker host |
| `mqtt.port` | `1883` | |
| `mqtt.username` / `mqtt.password` | `""` | |
| `mqtt.topic` | `teddyCloud` | root topic for everything |
| `mqtt.qos` | `0` | publish QoS |
| `mqtt.identification` | `""` | client-ID suffix |
| `mqtt.retain_will` | `TRUE` | |
| `mqtt.tls_enabled` | `FALSE` | plus `mqtt.tls_insecure` (`mqtt.c:312`) |

(`src/settings.c:330-344`)

```bash
curl -X POST "http://TEDDY/api/settings/set/mqtt.enabled?overlay=$CN" -d 'true'
curl -X POST "http://TEDDY/api/settings/set/mqtt.hostname?overlay=$CN" -d '192.168.1.10'
curl -X POST "http://TEDDY/api/settings/set/mqtt.port?overlay=$CN" -d '1883'
curl -X POST "http://TEDDY/api/settings/set/mqtt.topic?overlay=$CN" -d 'teddyCloud'
```

Subscriptions use **QoS 2** (`src/mqtt.c:296-301`).

### 9.3 Topics and commands

```c
// src/mqtt.c:981-982
osSprintf(ha_box_instance->id, "%s_Box_%s", settings_get_string("mqtt.topic"), box_id);
osSprintf(ha_box_instance->base_topic, "%s/box/%s", settings_get_string("mqtt.topic"), box_id);
```

`{cn}` = the box's **client-certificate common name** (`client_ctx->state->box.id`, `server.c:495`).
Per-box entities are only registered when `internal.overlayNumber != 0` (`mqtt.c:973-977`).
Command topics are matched with an **exact string compare** (`home_assistant.c:211-212`) — no
wildcards within a single command.

| Entity | Topic | Payload | Range / semantics |
|---|---|---|---|
| `CmdStop` | `%s/CmdStop/set` | **ignored** | kills ffmpeg server-side. No `stat_t`. |
| `CmdVolLimitSpk` | `%s/CmdVolLimitSpk/set` | `0`–`3` | `0=25% 1=50% 2=75% 3=100%` (`settings.c:305`) |
| `CmdVolLimitHdp` | `%s/CmdVolLimitHdp/set` | `0`–`3` | same scale (`settings.c:306`) |
| `CmdLed` | `%s/CmdLed/set` | `0`–`2` | `0=on 1=off 2=dimmed` (`settings.c:309`) |
| `CmdSlapEnabled` | `%s/CmdSlapEnabled/set` | `TRUE`/`FALSE` | `mqtt.c:919`: `!osStrcasecmp(payload,"TRUE")` |
| `CmdSlapDirection` | `%s/CmdSlapDirection/set` | `TRUE`/`FALSE` | `mqtt.c:942`; `TRUE` = back-left — **see the doc conflict in §11** |

**Payload parsing:**

```c
uint32_t val = (uint32_t)atoi(payload);            // mqtt.c:844, 869, 894
bool enabled = !osStrcasecmp(payload, "TRUE");     // mqtt.c:919, 942
```

- Numbers: `atoi`. `"abc"` → `0`; `"-1"` → `4294967295` (unsigned cast) → **rejected** by the
  `> 3` / `> 2` check.
- Booleans: **anything not equal to `TRUE` (case-insensitive) is `FALSE`.** Send exactly `TRUE`/`FALSE`.

**Enforcement in `tbs_cmd_*`:**

```c
// src/toniebox_state.c:141-157
bool tbs_cmd_stop(uint8_t overlay_id)
{
    if (overlay_id == 0 || overlay_id >= MAX_OVERLAYS) { return false; }
    toniebox_state_t *state = get_toniebox_state_id(overlay_id);
    if (state->box.stream_ctx.active && !state->box.stream_ctx.quit)
    {
        TRACE_INFO("Stopping active stream on overlay %" PRIu8 "\r\n", overlay_id);
        state->box.stream_ctx.active = false;
        sse_sendEvent("cmd", "stop", true);
        return true;
    }
    TRACE_INFO("No active stream to stop on overlay %" PRIu8 "\r\n", overlay_id);
    return false;
}
```

```c
// src/toniebox_state.c:159-174
bool tbs_cmd_set_vol_limit_spk(uint8_t overlay_id, uint32_t level)
{
    if (overlay_id == 0 || overlay_id >= MAX_OVERLAYS || level > 3) { return false; }
    settings_t *settings = get_settings_id(overlay_id);
    if (!settings->internal.config_used) { return false; }
    TRACE_INFO("Setting speaker volume limit to %" PRIu32 " on overlay %" PRIu8 "\r\n", level, overlay_id);
    settings_set_unsigned_id("toniebox.max_vol_spk", level, overlay_id);
    sse_sendEvent("cmd", "vol_limit_spk", true);
    return true;
}
```

| `tbs_cmd_*` | Line | Guard | Setting | `cmd` payload | Reaches box |
|---|---|---|---|---|---|
| `tbs_cmd_stop` | `:141` | `active && !quit` | *(none — kills ffmpeg)* | `stop` | **immediately** (server-side) |
| `..._vol_limit_spk` | `:159` | `level > 3` → reject | `toniebox.max_vol_spk` | `vol_limit_spk` | next freshness check |
| `..._vol_limit_hdp` | `:176` | `level > 3` → reject | `toniebox.max_vol_hdp` | `vol_limit_hdp` | next freshness check |
| `..._led` | `:193` | `mode > 2` → reject | `toniebox.led` | `led` | next freshness check |
| `..._slap_enabled` | `:210` | — | `toniebox.slap_enabled` | `slap_enabled` | next freshness check |
| `..._slap_dir` | `:227` | — | `toniebox.slap_back_left` | `slap_dir` | next freshness check |

**All reject `overlay_id == 0` and `overlay_id >= MAX_OVERLAYS`** (`MAX_OVERLAYS = 16 + 1`,
`include/settings.h:30`), and all five settings commands also require
`settings->internal.config_used` — i.e. the box must have a config yet. **Every failure mode emits
no SSE frame at all.**

**Immediate state echo** (`src/home_assistant.c:214-219`): after a command is received, the state
topic is republished synchronously — so `…/CmdLed` carries the stored value right after you publish
to `…/CmdLed/set`. `CmdStop` has **no** `transmit`, so you get only the SSE `cmd:stop`.

**Copy-paste commands:**

```bash
TOPIC=teddyCloud ; CN=3d8c0f13500304e0 ; BROKER=192.168.1.10

# watch everything for a box
mosquitto_sub -h "$BROKER" -t "$TOPIC/box/$CN/#" -v

# stop the server-side stream
mosquitto_pub -h "$BROKER" -t "$TOPIC/box/$CN/CmdStop/set" -m 'PRESS'   # payload ignored

# volume limits: 0=25% 1=50% 2=75% 3=100%
mosquitto_pub -h "$BROKER" -t "$TOPIC/box/$CN/CmdVolLimitSpk/set" -m '1'
mosquitto_pub -h "$BROKER" -t "$TOPIC/box/$CN/CmdVolLimitHdp/set" -m '3'

# LED: 0=on 1=off 2=dimmed
mosquitto_pub -h "$BROKER" -t "$TOPIC/box/$CN/CmdLed/set" -m '2'

# slap-to-skip
mosquitto_pub -h "$BROKER" -t "$TOPIC/box/$CN/CmdSlapEnabled/set" -m 'TRUE'
mosquitto_pub -h "$BROKER" -t "$TOPIC/box/$CN/CmdSlapDirection/set" -m 'FALSE'

# read state
mosquitto_sub -h "$BROKER" -t "$TOPIC/box/$CN/CmdVolLimitSpk" -v
```

**Server-wide settings** use a different pattern (`src/mqtt.c:1330-1334` +
`mqtt_settingname_clean()` at `:59-79`, which maps `.` → `-`):

```
{mqtt.topic}/{setting.name-with-dashes}/status     # state
{mqtt.topic}/{setting.name-with-dashes}/command    # write
```

e.g. `teddyCloud/toniebox-max_vol_spk/status`, `…/command`.

**HA discovery:** device id `{mqtt.topic}_Box_{cn}`; topics follow
`homeassistant/{type}/{device_id}/{entity_id}/config` (`MQTT_CONTROL.md:77-80`).

**Reconnect** (`MQTT_CONTROL.md:145-164`): exponential backoff 5 s → 60 s cap, reset on success,
automatic re-subscribe and re-publish, `disable_on_error` only logs after 10 failures
(`src/mqtt.c:44-45`, thread at `:1304-1310`).

### 9.4 REST control without MQTT

```c
// src/server.c:159
{REQ_POST, "/api/settings/set/", SERTY_WEB, &handleApiSettingsSet},
```

The **request body is the raw value** (no form encoding), the setting name is the path suffix, and
`?overlay=` selects the box (`src/handler_api.c:510-562`).

```bash
CN=3d8c0f13500304e0
curl -X POST "http://TEDDY/api/settings/set/toniebox.max_vol_spk?overlay=$CN" -d '2'   # → OK
curl -X POST "http://TEDDY/api/settings/set/toniebox.led?overlay=$CN"           -d '0'   # → OK
curl -X POST "http://TEDDY/api/settings/set/toniebox.slap_enabled?overlay=$CN"   -d 'true'
curl       "http://TEDDY/api/settings/get/toniebox.max_vol_spk?overlay=$CN"
curl       "http://TEDDY/api/settings/getIndex?overlay=$CN"
```

**Differences vs. MQTT:** the REST path writes the config but emits **no `cmd` SSE event** and **no
state-topic update**. And **neither path can stop a stream** — that requires MQTT `CmdStop` (or the
box reporting playback stopped).

**How the setting reaches the box:**

```c
// src/handler.c:660-672
void setTonieboxSettings(TonieFreshnessCheckResponse *freshResp, settings_t *settings)
{
    freshResp->max_vol_spk = settings->toniebox.max_vol_spk;
    freshResp->max_vol_hdp = settings->toniebox.max_vol_hdp;
    freshResp->slap_en = settings->toniebox.slap_enabled;
    freshResp->slap_dir = settings->toniebox.slap_back_left;
    freshResp->led = settings->toniebox.led;
    ...
}
```

Called from `src/handler_cloud.c:1100` immediately before packing the protobuf response to
`POST /v1/freshness-check`. This is the **only** delivery mechanism.

Wire contract — `proto/toniebox.pb.freshness-check.fc-response.proto:6-11`:

```proto
 6:   required int32 max_vol_spk = 3; // 0-3
 7:   required int32 slap_en = 4; // 1=on, 0=off
 8:   required int32 slap_dir = 5; // 1=back-left_forw-right, 0=forw-left_back-right
 9:   required int32 field6 = 6;
10:   required int32 max_vol_hdp = 7; // 0-3
11:   required int32 led = 8; // 0=on, 1=off, 2=dimmed
```

### 9.5 End-to-end radio recipe

**1. Pick the RUID** — the **byte-reversed** 64-bit UID printed on the tag
(`src/handler.c:638-658`):

```c
void getContentPathFromCharRUID(char ruid[17], char **pcontentPath, settings_t *settings)
{
    char filePath[18];
    osSprintf(filePath, "%.8s/%.8s", ruid, &ruid[8]);
    strupr(filePath);
    *pcontentPath = custom_asprintf("%s%c%s", settings->internal.contentdirfull, PATH_SEPARATOR, filePath);
}
```

e.g. UID `E0040…` → RUID `3D8C0F13/500304E0` → directory `3D8C0F13/500304E0`.

**2. Create the content JSON** (filesystem):

```bash
mkdir -p "$DATA/content/3D8C0F13/500304E0"
cat > "$DATA/content/3D8C0F13/500304E0.json" <<'EOF'
{
  "live": true,
  "nocloud": true,
  "source": "http://stream.example.org/live.mp3",
  "skip_seconds": 0,
  "cache": false,
  "cloud_ruid": "",
  "cloud_auth": [],
  "cloud_override": false,
  "tonie_model": "CUSTOM_RADIO",
  "hide": false,
  "claimed": false,
  "_version": 5
}
EOF
```

`$DATA` is `internal.datadirfull` (`GET /api/settings/get/internal.datadirfull`).
**`_version` must be `5`** (`CONTENT_JSON_VERSION`, `include/contentJson.h:73`) or the file is
rejected (`src/contentJson.c:137-140`).

**3. Or use REST** (no filesystem access) — `POST /content/json/set/<ruid>`,
`src/handler_api.c:4216-4324`. The URI **must be exactly 34 characters**
(`src/handler_api.c:4188-4192`):

```c
if (osStrlen(uri) != 34) { return ERROR_NOT_FOUND; }
char ruid[17];
osStrcpy(ruid, &uri[osStrlen(uri) - 16]);
```

Only six fields are accepted, each as one urlencoded value, into a `char item_data[256]`
(`:4236`) — **so `source` is capped at 255 characters**:

```bash
RUID=3d8c0f13500304e0
curl -X POST "http://TEDDY/content/json/set/$RUID?overlay=$CN" \
     --data-urlencode 'source=http://stream.example.org/live.mp3' \
     --data-urlencode 'live=true' \
     --data-urlencode 'nocloud=true' \
     --data-urlencode 'tonie_model=CUSTOM_RADIO'
curl "http://TEDDY/content/json/get/$RUID?overlay=$CN"          # read back
curl "http://TEDDY/content/json/3D8C0F13/500304E0.json?overlay=$CN"
```

**4. Register the model** (optional, but this is what makes `ContentTitle`/`ContentPicture` work) —
`docs/api/custom-tonies.md`:

```bash
curl -X POST http://TEDDY/api/toniesCustomJsonUpsert \
     -d 'model=CUSTOM_RADIO' \
     -d 'series=Internet Radio' \
     -d 'title=Radio' \
     -d 'tracks=Live' \
     -d 'audio_id[]=123456'
```

**Then reload the caches** or the next `ContentTitle` still says `"Unknown"`:

```bash
curl http://TEDDY/api/toniesJsonReload
```

This only populates `tonies.custom.json`; it carries **no** audio URL — the URL lives exclusively in
`content.json → source`.

**5. Arm the next unknown tag** (optional) — `src/handler_api.c:228-260`:

```bash
curl -X POST "http://TEDDY/api/assignUnknown" --data-urlencode 'path=3D8C0F13/500304E0'
```

The next unrecognised tag is auto-assigned to that content (`handler_cloud.c:528-616`), and
`core.flex_uid` (`:540-553`) always maps one special UID to the same content.

**6. Play it.** The box issues `POST /v1/freshness-check`, then `GET /v2/content/<ruid>`
(optionally with `Range`). teddyCloud spawns ffmpeg and streams the TAF back.

**7. Stop it:**

```bash
mosquitto_pub -h "$BROKER" -t "$TOPIC/box/$CN/CmdStop/set" -m 'PRESS'
# or simply take the figurine off the box -> playback:stopped -> toniebox_state.c:81-84
```

### 9.6 Stream tuning settings — `src/settings.c:286-292`

| Setting | Default | Effect on radio |
|---|---|---|
| `encode.bitrate` | `96` | kbps Opus |
| `encode.ffmpeg_stream_buffer_ms` | `2000` | pre-fill delay before the first byte reaches the box |
| `encode.ffmpeg_stream_restart` | `FALSE` | `true` = fresh stream on every resume (no duplicated audio) |
| `encode.ffmpeg_sweep_startup_buffer` | `TRUE` | discards the burst a station sends on connect (often ads) |
| `encode.ffmpeg_sweep_delay_ms` | `2000` | wait before starting to serve |
| `encode.stream_max_size` | `1024*1024*40*6-1` = **251658239** (~240 MB) | announced `Content-Length` **and** wall-clock limit (~6 h). *"The box may create an empty file this length for each stream. So if you have 10 streaming tonies you use, the box may block 10*240MB."* |

### 9.7 What does *not* work for streams

| Attempt | Why it fails |
|---|---|
| `POST /api/fileEncode` with a URL | `handler_api.c:2034-2039` requires `fsFileExists(multisource[i])` after prefixing with the content root — local files only |
| Setting `skip_seconds` via REST | not among the six fields in `handleApiContentJsonSet`; edit `content.json` on disk |
| `content.json → "cache": true` | `cache` is parsed and saved but **never read**; `cache.c` is used only for figurine **images** |
| `/reverseGeneric/…` as a radio proxy | `handler_reverse.c:57-61` hard-codes three targets |
| More than one concurrent radio per box | single `stream_ctx` per overlay |
| `GET /content/<ruid>` as the radio feed | `handleApiContent` (`handler_api.c:1672-1830`) only *reads* an existing file; it does not start ffmpeg |

### 9.8 REST endpoints for state re-read after a reconnect

Route table `src/server.c:104-182`; `?overlay=<ID>` where `ID` =
`settings->internal.overlayUniqueId`.

| Path | `server.c` | What it gives you |
|---|---|---|
| `GET /api/getBoxes` | `:155` | `{ID, commonName, boxName, boxModel}` — the `ID`s you need for `?overlay=`. **No live state.** |
| `GET /api/settings/getIndex?overlay=<ID>&internal=true` | `:157` | `internal.last_ruid`, `internal.last_ruid_time`, `internal.ip`, `internal.online`. Closest thing to a "last tag" readback — but **not** "currently placed" (§8.9 E). |
| `GET /api/settings/get/<name>?overlay=<ID>` | `:158` | scalar form, e.g. `internal.last_ruid` |
| `GET /api/getTagInfo?ruid=<ruid>&overlay=<ID>` | `:154` | `ruid`, `uid`, `type`, **`valid`**, `exists`, `live`, `nocloud`, `hide`, `claimed`, **`source`**, `trackSeconds[]`, `tonieInfo{model,series,episode,picture}`, `downloadTriggerUrl`, `audioUrl`. This is where you verify "is my stream actually configured for this tag". |
| `GET /api/getTagIndex?overlay=<ID>` | `:153` | index of all tag dirs, each enriched with the above |
| `GET /content/json/<ruid>?overlay=<ID>` | `:116` | raw sidecar — the stream definition |
| `POST /content/json/set/<ruid>?overlay=<ID>` | `:115` | **the write endpoint that aims a tag at a radio stream** |
| `GET /api/toniesJson` | `:145` | raw `tonies.json` |
| `GET /api/toniesCustomJson` | `:146` | raw `tonies.custom.json` — the table `ContentTitle`/`ContentPicture` resolve from |
| `POST /api/toniesCustomJsonUpsert` | `:147` | add/merge an entry (requires `model`) |
| `GET /api/toniesJsonReload` | `:144` | **required after editing `tonies.custom.json`** |
| `GET /api/tonieboxesJson` | `:150` | box *catalogue* — **not** a status endpoint |
| `GET /api/stats` | `:141` | server counters |

**No REST readback exists for:** `tag.audio_id`, any notion of "is a stream currently active", or
`tag.valid` / `tag.uid`. The only reconstruction available is
`internal.last_ruid` → ruid → `/api/getTagInfo` — i.e. *configured* state, not *live* state.
**Enable MQTT if live accuracy matters.**

---

## 10. Client contract and reference implementation

### 10.1 What a correct consumer must do

1. **Open with `GET /api/sse`** on the HTTP or HTTPS-web listener. No auth headers needed against a
   bare instance. A browser `EventSource` cannot set `Authorization` — cross-origin SSE carries no
   credentials.
2. **Use `addEventListener` per event name, not `onmessage`.** Every frame carries an `event:` line,
   so the default `message` type never fires for a registered name. Register all 18 or the missing
   ones are silently dropped.
3. **Parse `e.data` as JSON**, then read the envelope. `parsed.type` duplicates the event name.
   `parsed.data` is a **string** for everything except `rtnl-raw-log2` / `-log3` (**object**).
   **Numeric values arrive as strings** (`"42"`) — convert explicitly.
4. **Wrap `JSON.parse` in try/catch** and treat a failure as a dropped event (§3.3).
5. **De-chunk correctly** if not using `EventSource`: one SSE frame is up to 11 HTTP chunks; dispatch
   on the blank line (`\n\n`) after de-chunking; handle `\n`-only terminators.
6. **Use `keep-alive` as the liveness signal.** It arrives immediately and then every ~16 s. If
   nothing for ≳30 s, the connection is dead — tear down and reconnect. Do not rely on TCP keepalive
   (disabled) or any server-side idle timeout.
7. **Reconnect:** `EventSource` does it for you, but 503 is fatal for it, so implement your own
   backoff. Because there is **no `id:`, no `retry:`, no replay, and no `Last-Event-ID`**, every
   reconnect **loses everything that happened during the gap** ⇒ be **level-triggered /
   idempotent** and re-read authoritative state on every reconnect.
8. **Max 8 subscribers process-wide** (not per port). The 9th gets 503. Do not open one connection
   per event type — dedupe/fan out locally. Keep total web connections ≤ 31 per listener, since
   each stream pins a connection thread for its lifetime.
9. **Behind nginx/Apache/Cloudflare**, disable response buffering (`proxy_buffering off;`,
   `X-Accel-Buffering: no`) and set read timeouts well above 60 s.
10. **Avoid HTTP/1.0** — it downgrades the stream to close-delimited.
11. **SSE is strictly one-way.** To drive playback use MQTT (§9.3).
### 10.2 Reference consumer — browser, dependency-free

```js
// All 18 event names. `onmessage` never fires for these: every frame carries `event:`.
const SSE_EVENTS = [
  "keep-alive",
  "TagValid", "TagInvalid",
  "knock", "tilt",
  "playback",
  "cmd",
  "rtnl-raw-log2", "rtnl-raw-log3",
  "pressed", "charger",
  "ContentAudioId", "ContentTitle", "ContentPicture",
  "BoxTilt-A", "BoxTilt-B",
  "VolumeLevel", "VolumedB",
];

const state = {
  uid: null, valid: false,
  audioId: null, title: null, picture: null,
  playing: false, tiltA: 0, tiltB: 0,
  volumeLevel: null, volumedB: null,
  lastLogSeq: null, boxUptime: null,
};

function apply(name, raw) {
  let env;
  try {
    env = JSON.parse(raw);
  } catch {
    return;                       // unescaped payload broke the frame — drop it (§11.6)
  }
  const d = env.data;             // string, except the two raw events which are objects

  switch (name) {
    case "TagValid":   state.uid = d; state.valid = true;  break;
    case "TagInvalid": state.valid = false;                 break;
    case "playback":
      state.playing = d !== "stopped";
      if (d === "stopped") { state.audioId = state.title = state.picture = null; state.uid = null; }
      break;
    case "ContentAudioId": state.audioId = parseInt(d, 10);  break;
    case "ContentTitle":   state.title = d;                  break;
    case "ContentPicture": state.picture = d;                break;
    case "BoxTilt-A":  state.tiltA = parseInt(d, 10);       break;
    case "BoxTilt-B":  state.tiltB = parseInt(d, 10);       break;
    case "VolumeLevel": state.volumeLevel = parseInt(d, 10); break;
    case "VolumedB":    state.volumedB = parseInt(d, 10);    break;
    case "rtnl-raw-log2":
      // Guaranteed channel: tag removal, firmware, region, unknown future codes.
      state.lastLogSeq = d.sequence;
      state.boxUptime = d.uptime;
      if (d.function_group === 15 && (d.function === 8630 || d.function === 16067)) {
        onTagRemoved(d.field6);     // the ONLY lossless removal signal on SSE
      }
      break;
    case "cmd":
    case "pressed":
    case "charger":
    case "knock":
    case "tilt":
    default:
      break;
  }
  render(state);
}

function connect(url = "/api/sse") {
  const es = new EventSource(url);
  let lastSeen = Date.now();
  let attempt = 0;

  for (const name of SSE_EVENTS) {
    es.addEventListener(name, (e) => { lastSeen = Date.now(); apply(name, e.data); });
  }
  es.addEventListener("error", () => {
    // readyState === CLOSED means a non-200 (e.g. 503 when all 8 slots are taken).
    // EventSource will not retry that, so we do it ourselves.
    es.close();
    setTimeout(connect, Math.min(30000, 1000 * 2 ** attempt++));
  });
  setInterval(() => {
    if (Date.now() - lastSeen > 30000) { es.close(); attempt = 0; setTimeout(connect, 1000); }
  }, 5000);
  return es;
}
connect();
```

**Driving playback** is not possible from SSE — it is one-way. Publish MQTT commands instead (§9.3).

### 10.3 Reference consumer — raw HTTP, for non-browser clients

If you cannot use `EventSource` (no cookies/auth possible, want a `curl`-style pipeline), de-chunk
first and split frames on the blank line. Each SSE frame spans 7–11 HTTP chunks, so a chunk is
**not** a frame:

```bash
curl -N -H 'Connection: keep-alive' http://TEDDY/api/sse \
  | grep --line-buffered -E '^(event|data):'
```

Two terminal conditions: every frame ends with `\n\n` and uses bare `\n` (never `\r\n`) inside the
`event:`/`data:` lines.

---

## 11. Known bugs, hazards, and doc drift

Everything in this section is **[P]** — verified in the checked-out tree.

### 11.1 `read_big_endian64` sign-extends and collapses UIDs

```c
// src/handler_rtnl.c:165-168
int64_t read_big_endian64(const uint8_t *buf)
{
    return ((int64_t)read_big_endian32(buf)) | (((uint64_t)read_big_endian32(&buf[4])) << 32);
}
```

Two independent defects:

1. **Half-word swap** — the first four bytes become the *low* word. Not a plain BE64 read.
2. **Sign extension** — `read_big_endian32` returns `int32_t`; casting to `int64_t` sign-extends.
   When the first byte is ≥ `0x80` the value becomes `0xFFFFFFFF…` and the OR then **destroys the
   low word**, because `hi` is shifted into bits 32–63 which are already all ones.

Emulated exactly (32-bit wrap semantics, as on the target):

| `field6` payload | resulting `uid` | correct? |
|---|---|---|
| `000000020A0B0C0D` | `0A0B0C0D00000002` | yes |
| `D5E6F70801A2B3C4` | `FFFFFFFFD5E6F708` | **no** — should be `01A2B3C4D5E6F708` |
| `8000000000000001` | `FFFFFFFF80000000` | **no** — low word `00000001` lost entirely |

**Impact.** The `TagValid` / `TagInvalid` payload is `"%016"PRIX64` of this value
(`src/toniebox_state.c:34-36`), so a client sees `FFFFFFFF…`. The same value feeds
`getContentPathFromUID` (`src/handler.c:647-658`), `getTonieInfoFromUid` and therefore the
`ContentTitle` / `ContentPicture` model lookup — so for affected tags the title lookup resolves
against the wrong content path and returns `"Unknown"`.

**Collision.** Every payload of the form `8xxxxxxx…` maps to `FFFFFFFF80000000`-style values keyed
only by the first word — distinct tags with different low words collapse onto one uid. If you hit
this, treat the `TagValid` payload as a **half-swapped** value: take the 16 hex chars, swap the two
8-char halves, and if the result starts with `FFFFFFFF` reinterpret those bytes as an unsigned
32-bit low word. This also means `ruid` reconstruction (§6.2) must be done from the *raw*
`rtnl-raw-log2.field6` bytes, not from the `TagValid` string, when the first byte is ≥ `0x80`.

### 11.2 `sse_rawData()` never reports a write failure

```c
// src/handler_sse.c:138-159
error_t error = NO_ERROR;
for (each channel) { sseCtx->error = httpWriteString(...); ... }
return error;   // always NO_ERROR
```

The per-slot error is recorded in `sseCtx->error` but the **return value is hard-coded
`NO_ERROR`**. Consequences:

- `sse_sendEvent` / `sse_startEventRaw` / `rtnlEvent` can never detect or react to a dead peer, so
  the error-handling branches in `sse_sendEvent` and `sse_startEventRaw` are dead code.
- A dead subscriber is only reaped by the polling loop in `handleApiSse`
  (`src/handler_sse.c:61-96`), which notices on its next 100 ms tick. That is fine.
- One broken client does **not** abort the broadcast to the others — the loop continues, which is
  the desired behaviour. Just do not expect the producer to learn about failures.

### 11.3 Slot leak when the response header write fails

```c
// src/handler_sse.c:53-58
error_t error = httpWriteHeader(connection);
if (error != NO_ERROR)
{
    TRACE_ERROR("Failed to send header\r\n");
    return error;              // <-- sseCtx->active stays TRUE, sseSubscriptionCount never decremented
}
```

The slot was already claimed at `:38-42`. On this path it is never released, so **eight failed
header writes permanently exhaust SSE for the whole process** until teddyCloud restarts. Also note
`channel` is only initialised in `sse_init()` (`src/server.c:887`); it is never set in
`handleApiSse`, so a stale slot index is what gets logged.

### 11.4 Lock-order inversion between the SSE loop and the producers

- `handleApiSse` holds `MUTEX_SSE_CTX` for the whole loop body, then takes `MUTEX_SSE_EVENT` for the
  keep-alive write (`src/handler_sse.c:63`, `:82`).
- Every producer takes `MUTEX_SSE_EVENT` first (`sse_startEventRaw`, `:114`) and then `MUTEX_SSE_CTX`
  (`sse_rawData`, `:142`).

`CTX → EVENT` versus `EVENT → CTX` is a textbook inversion. The keep-alive window is short and the
critical sections are small, so it is unlikely to fire in practice, but it is a genuine deadlock
waiting for a scheduling coincidence. A producer (e.g. an RTNL box report) that lands in exactly
that window stalls the SSE thread until it is resolved.

### 11.5 `MUTEX_SSE_EVENT` is leaked on the error path

```c
// src/handler_sse.c:112-136
mutex_lock(MUTEX_SSE_EVENT);
error = sse_rawData("event: ");
if (error != NO_ERROR)
    return error;              // <-- returns while still holding the mutex
```

Currently unreachable only because `sse_rawData` always returns `NO_ERROR` (§11.2). If that is ever
fixed without fixing this, the **first** failed write deadlocks all SSE broadcasting permanently.
`MutexManager` has no recursive-lock mode for these, so the pairing with `sse_endEventRaw()`
(`:161-167`, which unconditionally unlocks) is load-bearing and fragile.

### 11.6 SSE payloads are not JSON-escaped

`escapeData == true` only wraps the payload in `"` (`src/handler_sse.c:179-201`). It performs **no**
escaping. `escapeString()` exists in the same file's sibling module but is used **only** for the
human-readable dump (`src/handler_rtnl.c:568`, `:587`), never for SSE.

Therefore any payload containing `"`, `\`, a newline or a control character produces a frame that
either fails `JSON.parse` or, worse, splits into a bogus extra SSE event. Affected events:

| Event | Source of untrusted text |
|---|---|
| `ContentTitle` | `item->title` from `tonies.json` — `src/handler_rtnl.c:392` |
| `ContentPicture` | `item->picture` — `src/handler_rtnl.c:395` |
| `ContentAudioId`, `BoxTilt-A/B`, `VolumeLevel`, `VolumedB` | server-formatted numbers, safe |

**Client rule:** always `try/catch` the parse and drop the frame on failure. Never assume a frame
boundary implies a successful parse.

### 11.7 Fixed 4096-byte stack buffer in `rtnlEvent()`

```c
// src/handler_rtnl.c:205
char_t buffer[4096];
...
osSprintf(&buffer[i * 2], "%02X", rpc->log2->field6.data[i]);   // line 568 area / :221-226
```

`field6` and `field9` are protobuf `bytes` with no application-level cap. `handleRtnl` only requires
`buffer[pos]` and `buffer[pos + 1]` to be zero, i.e. a frame length **< 0x10000**
(`src/handler_rtnl.c:106-111`), so a ~64 KB payload is accepted and hex-encoded into a 4096-byte
stack buffer. That is a **stack buffer overflow** reachable by anything that can `POST` to
`/*binary` — a compromised or spoofed box on the API port. Not exploitable from the SSE side, but
worth knowing when you expose the API port.

### 11.8 `SSE_TIMEOUT_S` is not an idle timeout

```c
// src/handler_sse.c:64
if (... || (sseCtx->lastConnection + SSE_TIMEOUT_S < time(NULL)))
```

`lastConnection` is refreshed on **every successful write**
(`:91`, `:153`), and the keep-alive fires every ~16 s regardless of traffic. So the 60 s teardown
only ever triggers when writes have been *failing* for a minute — it is a **dead-peer reaper**, not
a lifetime cap. A connected-but-idle client holds its slot forever.

**DoS note:** because the slots are process-global and capped at 8, eight clients that connect and
never read will lock out every other SSE consumer, including the teddyCloud frontend's own live
view. Run a reverse proxy with connection limits in front if the port is reachable.

### 11.9 `sse_keepAlive()` is dead code

Declared (`include/handler_sse.h:34`) and defined (`src/handler_sse.c:208-211`) but never called —
the keep-alive is written inline at `:83` instead. Harmless, but do not use it as a model for
emitting events: it bypasses the per-slot error bookkeeping that `sse_rawData` provides.

### 11.10 Doc drift and settings traps

| Claim | Reality |
|---|---|
| `sse_keepAlive()` provides heartbeats | unused; keep-alive is inline (`§11.9`) |
| RTNL is a two-way control channel | it is one-way; control is MQTT (`MQTT_CONTROL.md:9-20`) |
| `ContentAudioId` proves a radio is playing | not proven; firmware-dependent (`§8.4`) |
| `internal.last_ruid` = currently placed tag | it is **last ruid seen**, rewritten on removal (`§8.9`) |
| 503 will be retried by `EventSource` | a non-200 is **fatal** for `EventSource` (`§2.4`) |
| `encode.ffmpeg_stream_restart` = "force restart" | default `FALSE` (`src/settings.c:288`) |
| `encode.stream_max_size` default | `40 MB × 6 − 1` = 251,658,239, min 1 MB (`src/settings.c:291`) |
| `encode.ffmpeg_stream_buffer_ms` | 2000 ms default, 0–60000 (`src/settings.c:287`) — this is the added latency before bytes reach the box |

---

## 12. Source index

| File | What it decides for this document |
|---|---|
| `src/handler_sse.c` | The entire SSE implementation: slots, keep-alive, frame writes, `escapeData` |
| `include/handler_sse.h` | `SSE_MAX_CHANNELS 8`, `SSE_TIMEOUT_S 60`, `SSE_KEEPALIVE_S 15`, API |
| `src/server.c` | Route table (`:165`), auth callback (`:660`), 404 fallback, `sse_init()` (`:887`), 31-connection cap (`:909`) |
| `src/server_helpers.c` | `httpServerUriNotFoundCallback` (404 body), `resolveSpecialPathPrefix` |
| `src/handler.c` | `httpWriteString` (`:1037`), uid↔ruid helpers (`:638`, `:647`), content-path selection |
| `src/toniebox_state.c` | `TagValid`/`TagInvalid` (`:36`), `knock`/`tilt` (`:60`,`:65`), `playback` (`:69`), stream stop (`:81`), `cmd` |
| `include/toniebox_state_type.h` | Playback / system-sound enums, per-box `stream_ctx` |
| `src/handler_rtnl.c` | Raw feed (`:203`), RTNL group 15/12/20/22/27 mapping, `Content*`, `BoxTilt-*`, `Volume*` |
| `include/handler_rtnl.h` | Function-group and function-code enums (`:16`, `:87`) |
| `proto/toniebox.pb.rtnl.proto` | log2 / log3 field layout; explains the missing JSON key `7` |
| `src/handler_cloud.c` | `CT_SOURCE_STREAM` (`:638`) and `CT_SOURCE_TAP_STREAM` (`:689`) lifecycles, audio-id deduction (`:699`, `:904`) |
| `src/toniefile.c` | `ffmpeg_stream` TAF header + audio id (`:745`), `stream_max_size` (`:743`), task teardown (`:869`) |
| `src/tonie_audio_playlist.c` | TAP playlist render, `quit` flag (`:224`) |
| `src/contentJson.c` | `_source_type` classification (`:97`), `CT_SOURCE_STREAM` forcing `live`/`nocloud` |
| `include/contentJson.h` | `ct_source_t` enum, `CONTENT_JSON_VERSION 5` |
| `src/mqtt.c` | Command topics, range validation, Home Assistant entities, `MQTT` reconnect |
| `MQTT_CONTROL.md` | The documented control surface and its stated limitations |
| `src/handler_api.c` | `POST /content/json/set/<ruid>`, `GET /api/toniesJson*`, state re-read endpoints |
| `src/settings.c` | `log.level` (`:63`), `toniebox.*` limits (`:305-309`), `encode.*` stream tuning (`:286-292`), `mqtt.*` (`:330`) |
| `src/cyclone/cyclone_tcp/http/http_server.c` | `httpWriteStream` chunking (`:838`), content send loop (`:1167`) |
| `src/cyclone/cyclone_tcp/http/http_server_misc.c` | Response-header rendering (`:786`), HTTP/1.0 chunking disable (`:805`) |
| `contrib/data/www/legacy.html` | Reference JS consumer: hex→uid half-swap (`:1653`), function handlers (`:1649`) |
| `docs/api/custom-tonies.md` | How to point a tag at a radio URL |

### Reproducing the analysis

```bash
git -C /var/home/jan/ws/teddycloud rev-parse HEAD      # 78c3b38 (v0.7.0)
# every claim above is a file:line reference into that tree
```

To confirm the empirical items ([E] — §8.4 audio-id reports for streams, log3 `datetime` encoding,
raw-log cadence) set `log.level=6`, `rtnl.logRaw=true`, `rtnl.logHuman=true` and watch
`rtnl-raw-log2` / `rtnl-raw-log3` on a real box.
