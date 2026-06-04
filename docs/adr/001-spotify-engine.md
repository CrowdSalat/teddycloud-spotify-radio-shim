# ADR-001: Use go-librespot as Spotify Engine

**Status:** Accepted  
**Date:** 2026-06-04

## Context

The shim needs a headless Spotify playback engine that can be controlled programmatically (play URI, pause, resume, skip), output decoded audio to a pipe (no sound card), persist session credentials across restarts, and run inside an OpenShift container without a GUI.

## Options Considered

1. **go-librespot** — Open-source Spotify Connect client written in Go. REST API + WebSocket events. Pipe audio backend. Interactive OAuth flow. Official Docker image.
2. **librespot (Rust)** — The original open-source Spotify library. Mature, but pipe output is less documented. No built-in REST API — would need a wrapper.
3. **Spotify Web API + official SDK** — Cloud-based control. Requires a *separate* playback device (Spotify Connect target). Cannot decode audio locally — only controls remote playback. Does not produce an audio stream we can pipe.

## Decision

**go-librespot.**

## Rationale

Every PRD requirement maps directly to a go-librespot feature:

| Requirement | go-librespot | Gap? |
|---|---|---|
| Play a Spotify URI | `POST /player/play {"uri": "..."}` | None |
| Pause / resume / skip | `POST /player/pause`, `/resume`, `/next`, `/prev` | None |
| Real-time playback state | WebSocket `/events` — `playing`, `paused`, `will_play`, `metadata` | None |
| Audio to pipe (RAM-only) | `audio_backend: pipe`, outputs PCM to named FIFO | None |
| Session persistence | `state.json` in configurable `-config_dir` → mount on PVC | None |
| OAuth login flow | `credentials.type: interactive` → browser-clickable auth URL | None |
| Headless / no GUI | CLI daemon, no X11/Wayland | None |
| Container-ready | Official Dockerfile, statically-linkable Go binary | None |

The Rust `librespot` would require building a REST API wrapper around it. The Spotify Web API cannot produce a local audio stream at all.

## Consequences

- The shim depends on an upstream open-source project with no SLA. Monitor releases.
- Pipe output emits **raw PCM** (s16le/s32le/f32le), not a container format. The shim must handle transcoding or raw-PCM delivery to Teddycloud.
- The pipe driver has a [known deadlock bug](../research/go-librespot-pipe-deadlock.md) — addressed in [ADR-005](005-fifo-backpressure.md).
- Hardcoded 44,100 Hz stereo. Wire rate: 176,400 bytes/sec at s16le.
