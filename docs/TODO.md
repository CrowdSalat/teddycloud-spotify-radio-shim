# TODO

Deferred ideas and open evaluations that do not belong in the current implementation scope.

---

## OGG API — Non-Radio Tonie Content from Arbitrary Sources

### Background

The Toniebox content flow for normal (non-radio) Tonies:

1. Box reads NFC tag UID
2. Box requests content URL from Teddycloud: `GET /v1/content/{uid}`
3. Teddycloud returns a URL pointing to an OGG audio file
4. Box downloads the full OGG (with chapter markers per track) to internal flash
5. Box plays from local cache — skip/prev navigates chapters natively, works offline

The box does not care what is behind the URL. As long as the endpoint returns a valid OGG file, the source is transparent.

### The Idea

Expose arbitrary audio sources as an OGG HTTP API and register them as normal Tonie content in Teddycloud. The box downloads and caches the result exactly like a physical Tonie.

Candidates that could sit behind such an API:

- **NAS / NFS** — serve your own ripped albums as OGG
- **Spotify album / playlist** — encode on demand via go-librespot, expose as OGG download
- **Podcast feeds** — package episodes as OGG with chapter markers
- Any audio source that can be encoded to OGG/Vorbis

This would complement the streaming shim rather than replace it:

| Mode | Use case | Connectivity |
|---|---|---|
| **Streaming shim** (current project) | Live / dynamic / radio-style | Always online |
| **OGG API** | Catalogued albums, static playlists | Download once, play offline |

### What Needs Evaluating

1. **External URL support in Teddycloud** — does the content URL returned to the box have to be served by Teddycloud itself, or can it be an arbitrary external HTTPS URL? If Teddycloud supports external URLs, the OGG API can live anywhere.

2. **Cache invalidation** — does the box re-download on every figurine placement, or does it cache indefinitely? If it re-validates (ETag / Last-Modified), a dynamic OGG API (e.g. a Spotify playlist that changes) stays fresh. If it caches aggressively, content is stale until manually cleared.

3. **OGG chapter format** — confirm the exact OGG/Vorbis chapter structure the Toniebox expects. Each Spotify track would be one chapter. Skip/prev on the box would then navigate tracks without any shim involvement.

4. **Spotify ToS** — streaming is permitted; downloading / caching Spotify content is not. An OGG API backed by Spotify would be legally greyer than the streaming shim. NAS/NFS-backed content (own media) has no such concern.

### Outcome

If Teddycloud supports external content URLs and the box re-validates on placement, the OGG API approach is a clean second mode: configure `{uid} → https://ogg-api/album/xyz.ogg` in Teddycloud, the box handles everything else natively.
