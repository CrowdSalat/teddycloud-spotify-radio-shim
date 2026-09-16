# Research: streaming pipeline end-to-end (2026-09-16)

All findings on how a Spotify-playing Toniebox works through teddycloud + the
shim, how teddycloud encodes audio, why radio streams play cleanly while our
shim stutters, and every fix tried so far with measured results.

Source of truth for the teddycloud internals: `teddycloud` git checkout
(`src/handler_cloud.c`, `src/toniefile.c`, `src/cyclone/.../http_server.c`).

---

## 1. End-to-end pipeline

### 1.1 Streaming tonie (Spotify via shim)

```
Spotify app/player
   │  (playback_events, commands over WS)
   ▼
Soloist (headless Spotify client, PulseAudio sink "virtual_out")
   │  PCM 22050 Hz s16le stereo
   ▼
PulseAudio "virtual_out.monitor" ← shim recorder (non-blocking, drop-on-full)
   │  chunks 4096 B
   ▼
shim /stream  →  WAV container, 22050 Hz, 88.2 kB/s  (HTTP, chunked, no CL)
   │  ▲ fat leg (88.2 kB/s); proven non-limiting by STATIC_STREAM diagnostic (§4)
   ▼
ffmpeg (spawned by teddycloud): decode WAV → resample to 48000 Hz s16le → stdout pipe
   │  PCM 48000 Hz s16le stereo 1536 kbit/s (in-memory/popen)
   ▼
teddycloud toniefile_encode: Opus encoder 48 kHz stereo, 60 ms frames, VBR → TAF
   │  Opus Ogg TAF written to disk (only ~1-2 min ahead, streaming)
   ▼
Toniebox pulls the TAF via HTTP range requests (206, fake Content-Length
   │  = stream_max_size, box sees preallocated 240 MB)
   ▼
Toniebox decodes Opus natively, plays through speaker
```

### 1.2 Local content (TAF on disk, e.g. classic figurine)

```
TAF file on disc (pre-encoded Opus Ogg) → served directly with HTTP range
requests → Toniebox. No ffmpeg, no re-encode, no recording step.
```

### 1.3 Web radio (TonieBox "radio")

```
Radio URL (AAC 64 kbps etc.) → ffmpeg decode → resample to 48 kHz → PCM →
teddycloud Opus encode → TAF → box pulls TAF. Identical code path to §1.1,
only the source URL and its byte rate differ.
```

---

## 2. Commands teddycloud uses

### 2.1 The decode step (`toniefile.c:586-613`)

```
ffmpeg -i "<source>" -f s16le -acodec pcm_s16le -ar 48000 -ac 2 -ss <skip_seconds> -
```

- `<source>` is arbitrary ("anything ffmpeg can decode (urls)"): our
  `http://teddycloud-spotify-shim:8080/stream?...` or a radio URL.
- For an incomplete/TAF source with known byte offset the command is
  `tail -c +<off> "<source>" | ffmpeg -i - -f s16le -acodec pcm_s16le -ar 48000 -ac 2 -`.
- Pure **resample + PCM passthrough**, no compression on this leg.
- Output is raw s16le 48000 Hz stereo on the pipe, read with `fread`
  (`ffmpeg_decode_audio`, `toniefile.c:665-696`).

### 2.2 The encode step (`toniefile_create` + `toniefile_encode`, `toniefile.c:95-526`)

- Opus encoder: `opus_encoder_create(48000, 2, OPUS_APPLICATION_AUDIO)`.
- Config: `encode.bitrate` kbit/s, VBR on, expert frame duration
  `OPUS_FRAMESIZE_60_MS` (`include/toniefile.h:6-10`, `settings.c`).
- Input PCM is 48000 Hz stereo; 60 ms = 2880 samples/frame; output Ogg/Opus.
- In streaming mode the TAF is written with a fake total size
  (`stream_max_size`, default ~240 MB, `settings.c`) so the box believes it
  is a bounded file.

### 2.3 Serving to the box (`httpSendResponseStream`, `http_server.c:945-1159`)

- Streamed TAF is served like a file: HTTP 200 or 206 Range, `Content-Length`,
  **no** chunked encoding.
- For ranges, file access is offset by `TONIE_HEADER_LENGTH` (0x1000) and the
  `taf_chapter_split` offset machinery handles chapter boundaries.
- `stream_max_size` is used as the advertised length for streams
  (`ffmpeg_stream_restart` controls whether a continued stream restarts the
  file or appends).

---

## 3. Difference: shim stream vs web radio vs local content

| Aspect | Local TAF | Web radio | Shim (Spotify) |
|--------|-----------|-----------|----------------|
| Encode | none (pre-encoded) | live Opus encode | live Opus encode |
| ffmpeg involved | no | yes | yes |
| Source byte rate | n/a (disk) | ~8 kB/s (AAC 64 kbps) | ~88 kB/s (WAV 22050) |
| HTTP leg under teddycloud ceiling (~136 kB/s) | n/a | 6 % | 65 % |
| Resulting playback pace | instant | 1.11× (measured) | 0.89× (measured) |

Key insight: teddycloud's ffmpeg step *always* decodes the source to PCM and
*always* re-encodes to Opus for the box. The box never consumes the shim's
raw bytes. The bottleneck is the intermediate HTTP leg between the source and
ffmpeg: the box/engine have ample headroom whenever that leg is compressed.

---

## 4. What we tried so far, with results

All in the deployed OpenShift pod (`teddycloud-spotify-shim`), playing a
Spotify tonie on the real Toniebox. Measure: ffmpeg `speed` (reading pace vs
real-time) and `drop_ratio` (chunks discarded by recorder drop-on-full).

| Version | Change | ffmpeg speed | drop_ratio |
|---------|--------|--------------|------------|
| v0.1.3 | baseline capture 44100 Hz, 4 KiB segments | 0.475× | ~0.50 |
| v0.1.4 | batch /stream writes to 16 KiB segments | 0.73× | ~0.29 |
| v0.1.5 | batch to 128 KiB segments | 0.77× | ~0.26 |
| v0.1.6 | halve capture rate to 22050 Hz (88.2 kB/s) | 0.89× | ~0.17 |
| v0.1.7 | STATIC_STREAM=true (synthesized tone, unpaced) | 36.9× direct / 75–100× real-box | 0 (recorder bypassed) |

The 4→16 KiB segment gain (0.475→0.73×) led to a *conjectured* consumer read
ceiling of ~136 kB/s (16→128 KiB and half-rate capture plateau). **The v0.1.7
result disproves that ceiling** — see §4a: with the recorder removed from the
path, the exact same consumer (ffmpeg + teddycloud + box) reads the stream at
75–100×.

### Current symptom (v0.1.6)

- Recorder produces exactly real-time (~21.5 chunks/s = 88.2 kB/s) — healthy.
- Consumer (ffmpeg) pulls at ~76.8 kB/s → 0.89×: the drop-valve still throws
  away ~17 % of chunks while playing (audible jumps).
- After ~2 min the cumulative deficit exhausts the box's read-ahead buffer;
  ffmpeg aborts ("Conversion failed!"), Toniebox shows an error.
- Control: the same box plays a web radio (AAC 64 kbps) through the **identical**
  teddycloud code path at 1.11× without drops.

## 4a. STATIC_STREAM diagnostic (v0.1.7) — isolates the bottleneck

**Hypothesis to test:** is the ~0.89× ceiling (a) teddycloud's HTTP ingest (a
~136 kB/s read ceiling) or (b) the shim's live pacing (recorder drop-on-full)?

**Method:** `STATIC_STREAM=true` swaps `/stream`'s source from the live
PulseRecorder to a Go goroutine that synthesizes a 440 Hz sine WAV and produces
(as fast as the consumer drains). No recording, no pacing, no drop-valve — the
shim is no longer the production rate-limiter.

**Result (2026-09-16, real Toniebox + real teddycloud):**
- Direct probe `ffmpeg -i http://teddycloud-spotify-shim:8080/stream...` from
  inside the teddycloud pod: **speed=36.9×** (≈3.25 MB/s), sustained.
- Playback on the real Toniebox (teddycloud's identical encode path):
  ffmpeg **75–100×**, shim `streams=1 delivered_kB_s≈2000–19000`, zero drops,
  box played the tone cleanly.
- Same box, same teddycloud, same network as the 0.89× runs — only the shim's
  producer differed.

**Conclusion:** the HTTP ingest leg and the box are NOT the bottleneck.
teddycloud's ffmpeg can read /stream at >3 MB/s when there's no production
bottleneck. The ~0.89× ceiling therefore comes entirely from the shim's **live
recorder path**: production at exactly real-time, delivered against a tiny
8-chunk (~186 ms) buffered channel with drop-on-full. This matches the
decision rule: static ≥1.0× → **fix shim buffering/pacing** (Step 3), NOT
AAC encoding.

### Full history and root-cause write-ups

See `docs/research/ocp-playback-issues.md` (drop-on-full, segment-size and
sample-rate investigations, §1–1c) for the detailed analysis.

---

## 5. Direction (revised after §4a): fix shim buffering/pacing first

§4a proves the shim's live recorder path is the sole bottleneck — its read
channel gives the consumer only ~186 ms of slack before chunks are dropped.
Fix options, in order of preference:

1. **Bigger recorder buffer / slack** — raise `defaultBufferLen` (8 chunks →
   seconds of PCM) so transient consumer stalls are absorbed instead of
   dropping. Cheapest, no format change.
2. **Remove drop-on-full for slow consumers** — block or hold in a second
   stage buffer instead of discarding, at the cost of live-lag.
3. **Fallback if buffering proves insufficient:** AAC encoding in the shim
   (§5.1) — reduce the leg to ~8–16 kB/s.

## 5.1 Fallback: encode AAC in the shim

Swap the shim's raw WAV for AAC so the fat HTTPS leg goes from ~88 kB/s to
~8–16 kB/s, reproducing the radio's margin:

| Source | Byte rate | Margin vs radio (8 kB/s) |
|--------|-----------|---------------------------|
| WAV 22050 Hz | 88 kB/s | 11× the radio's rate (tight) |
| AAC-LC 64 kbps | 8 kB/s | = radio's rate (comfortable) |
| AAC-LC 96–128 kbps | 12–16 kB/s | 1.5–2× (comfortable) |

`ffmpeg -i <aac-url> -f s16le -ar 48000 -ac 2 -ss 0 -` decodes AAC-LC cleanly;
the `<source>` field is already arbitrary "anything ffmpeg can decode", so no
teddycloud change is required.

### Open question

What does teddycloud do if `encode.bitrate` is raised, and is the Opus
re-encode avoidable at all? As-is the shim → ffmpeg → teddycloud(Opus) path is
mandatory because the box consumes Opus TAF natively — there is no way for the
shim to reach the box directly. AAC is thus purely an optimisation of the one
leg (shim → ffmpeg), keeping teddycloud's encode step unchanged.