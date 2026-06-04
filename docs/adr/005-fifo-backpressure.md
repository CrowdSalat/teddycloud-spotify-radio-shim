# ADR-005: Backpressure-Safe FIFO Reader with Discard Safety Valve

**Status:** Accepted  
**Date:** 2026-06-04

## Context

go-librespot's pipe output driver has a [mutex deadlock bug](../research/go-librespot-pipe-deadlock.md): the `outputLoop` holds `out.lock` across a blocking `file.Write()` to the FIFO. If the FIFO buffer is full and nobody is reading, `Write()` blocks forever — and since all player control methods (`Pause`, `Resume`, `Close`) need the same lock, they deadlock permanently. No upstream fix exists.

However, the deadlock condition is narrower than it appears. The `outputLoop` releases `out.lock` between every iteration. During normal playback, `Write()` blocks only briefly (~46ms per 8 KB chunk at 44.1 kHz stereo s16le) until the consumer reads one chunk, then the lock is released. **Brief FIFO fullness is normal backpressure, not a deadlock.** The true deadlock occurs only when the consumer stops reading entirely for >~300ms.

The question is how to design the FIFO reader to prevent this.

## Options Considered

1. **Always-drain (keep FIFO empty)** — A dedicated goroutine reads the FIFO at maximum speed into a ring buffer, regardless of consumer speed. go-librespot runs at full decoder speed (faster than real-time), wasting CPU and network.
2. **Backpressure-safe reader** — Let the FIFO provide natural backpressure (rate-limits go-librespot to real-time). Reader normally reads at consumer speed. If the downstream HTTP consumer stalls, reader switches to discard mode — keeps reading FIFO but drops data. This prevents the deadlock without wasting resources during normal playback.
3. **Fork go-librespot** — Fix the mutex bug directly (release `out.lock` before `file.Write()`). Eliminates the constraint at the source.

## Decision

**Backpressure-safe reader with discard safety valve.**

## Rationale

**Option 1 rejected:** go-librespot would decode and download at unlimited speed because no backpressure exists. This wastes CPU, burns through Spotify CDN bandwidth, and requires a ring buffer large enough to hold an entire song (~31 MB for a 3-minute track). The FIFO's natural rate-limiting is correct and desirable.

**Option 3 rejected (for now):** The mutex protects concurrent access to `out.paused`, `out.closed`, `out.volume`, and `out.reader`. Releasing it before `Write()` requires refactoring to avoid data races — not a one-line fix. Maintaining a fork means rebasing on every upstream release. If upstream shows no intent to fix, a carefully-reviewed PR becomes worthwhile later.

**Option 2 is correct:** During normal playback, the FIFO is mostly full (~56 KB of 64 KB). This is healthy — it rate-limits go-librespot to ~1× real-time. The reader reads at the speed the HTTP consumer pulls, which matches real-time playback. `Write()` blocks briefly (~46ms), lock is released, API calls succeed in the gap. **No wasted resources.**

The safety valve activates only when the downstream HTTP consumer stalls:

```
Consumer stalls → TCP buffer full → HTTP Write blocks
→ ring buffer fills (~1.5s) → reader select/default triggers
→ DISCARD MODE: reader keeps consuming FIFO, drops data
→ FIFO never stays full → Write() never blocks long → lock released
→ API calls succeed → ✅ no deadlock
```

### Data path (three buffers)

```
go-librespot ──▶ [FIFO: 64 KB] ──▶ [reader goroutine] ──▶ [chan: 256 KB] ──▶ [HTTP writer] ──▶ Teddycloud
                  kernel buffer      reads at consumer      32 × 8 KB slots    blocks on TCP
                  rate-limiter        speed; discards        decoupler +        (gates real-time
                  (mostly full        on downstream          safety valve        consumption)
                   = normal)          stall
```

### Reader goroutine invariant

**Never stop reading the FIFO for >~300ms.** This is trivially satisfied during normal playback (reader reads at real-time, FIFO provides backpressure with ~46ms lock holds). The invariant is only threatened by downstream stalls — handled by the discard path.

## Consequences

- The FIFO reader goroutine is the system's critical safety component. If it panics or blocks, go-librespot deadlocks within ~360ms. Must be implemented with pre-allocated buffers and panic recovery.
- Ring buffer implemented as a buffered Go channel (`make(chan []byte, 32)` → 32 × 8 KB = 256 KB). Non-blocking send via `select/default` for discard mode.
- During pause (via `POST /player/pause`), go-librespot stops writing to the FIFO. Reader blocks on an empty FIFO — idle, zero CPU. No discard needed.
- During skip, stale data in the channel is flushed when go-librespot's WebSocket emits `will_play` for the new track.
- Monitor upstream for a fix. If the deadlock is patched, the discard safety valve becomes unnecessary but the architecture remains correct.
