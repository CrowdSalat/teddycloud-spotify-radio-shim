# ADR-004: Unified API Control over Hybrid Audio/Control Split

**Status:** Accepted  
**Date:** 2026-06-04

## Context

The shim must map Toniebox hardware events (figurine lifted/placed, ear slaps) to Spotify playback commands (pause, resume, skip). Two strategies were considered for how the shim interacts with go-librespot.

## Options Considered

1. **Hybrid** — The audio channel (FIFO → HTTP) handles pause/resume implicitly (stop reading FIFO = pause; resume reading = resume). The control channel (SSE) is used only for skip commands via go-librespot's REST API.
2. **Unified** — The audio channel is a pure data pipe. ALL commands (play, pause, resume, next, prev) go through go-librespot's REST API. The FIFO is always being read.

## Decision

**Unified. All playback control via go-librespot REST API.**

## Rationale

The Hybrid model is **non-functional** due to the [go-librespot pipe deadlock](../research/go-librespot-pipe-deadlock.md):

- Hybrid pause = stop reading FIFO → FIFO kernel buffer fills in ~360ms → go-librespot's `file.Write()` blocks while holding `out.lock` → **all API calls deadlock permanently** (pause, resume, next, prev, play, stop) → process must be killed.
- This is deterministic, not a race condition. Every figurine-lift triggers it.
- Skip commands also deadlock because they enter the same `Player.manageLoop` pipeline.

Even without the deadlock, the Hybrid model has a second fatal flaw: **no track boundary detection in raw PCM**. After a skip, the FIFO contains interleaved old-track and new-track PCM with no frame markers. The shim cannot detect where one ends and the other begins.

The Unified model avoids both problems:
- All commands go through go-librespot's API, which acquires `out.lock` in the gap between `outputLoop` iterations (~46ms). Commands succeed reliably.
- Track boundaries are signaled explicitly via go-librespot's WebSocket `will_play` event.
- Single source of truth: `GET /status` always reflects the actual playback state.

## Consequences

- The FIFO reader goroutine must never stop reading — see [ADR-005](005-fifo-backpressure.md).
- The shim subscribes to go-librespot's WebSocket `/events` for `will_play` signals (used to synchronize track transitions and flush stale data from the ring buffer).
- Event-to-API mapping:

| Hardware Event | go-librespot API Call |
|---|---|
| Figurine placed | `POST /player/resume` |
| Figurine lifted | `POST /player/pause` |
| Right slap (knock forward) | `POST /player/next` |
| Left slap (knock backward) | `POST /player/prev` |
| New `/stream?uri=X` request | `POST /player/play {"uri":"X"}` |
