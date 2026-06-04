# go-librespot: Pipe Backend Pause Deadlock

## Summary

When using go-librespot with the `pipe` audio backend, **pause commands (and any other player commands) will deadlock if the pipe's buffer is full** and the consumer is not reading fast enough. This affects all control interfaces: the REST API, Spotify Connect, and MPRIS.

## Root Cause

The pipe output driver (`output/driver-pipe.go`) holds a mutex (`out.lock`) for the entire duration of its write loop iteration, including the blocking `file.Write()` call. When the named pipe (FIFO) buffer is full, `Write` blocks until a consumer drains data — but the mutex is never released.

All control methods (`Pause()`, `Resume()`, `Close()`) attempt to acquire the same mutex, so they block indefinitely while the write is stalled.

### Deadlock Chain

```
HTTP POST /player/pause (or Spotify Connect pause, MPRIS, etc.)
  → daemon.AppPlayer.pause()
    → player.Player.Pause()              // sends cmd, waits for response
      → Player.manageLoop: out.Pause()   // tries to acquire out.lock — BLOCKED
        → pipeOutput.outputLoop:
            out.lock.Lock()              // held
            out.reader.Read(...)
            out.file.Write(...)          // blocked on full pipe, lock never released
```

### Relevant Code

**`outputLoop`** — holds mutex across the blocking write:

```go
func (out *pipeOutput) outputLoop() {
    for {
        out.lock.Lock()
        for out.paused && !out.closed {
            out.cond.Wait()
        }
        // ...
        n, err := out.reader.Read(floats)
        // ...
        _, err := out.file.Write(bytes[:nn]) // blocks when pipe is full
        // ...
        out.lock.Unlock()
    }
}
```

**`Pause`** — waits for the same mutex:

```go
func (out *pipeOutput) Pause() error {
    out.lock.Lock() // deadlocks here
    defer out.lock.Unlock()
    out.paused = true
    out.cond.Signal()
    return nil
}
```

Additionally, the pipe is explicitly set to blocking mode after the initial non-blocking open:

```go
out.file, err = os.OpenFile(opts.OutputPipe, os.O_WRONLY|syscall.O_NONBLOCK, 0)
// ...
syscall.SetNonblock(int(out.file.Fd()), false) // restore blocking mode
```

## Impact

- **`POST /player/pause`** hangs indefinitely.
- **All other player commands** (`resume`, `seek`, `next`, `stop`, `volume`, etc.) also hang because go-librespot's `Player.manageLoop` is single-threaded — it processes one command at a time, and it's stuck waiting for `out.Pause()` to return.
- **Spotify Connect commands** are equally affected since they go through the same player command pipeline.
- The only way to recover is to either resume reading from the pipe (unblocking the write) or kill the process.

## Workaround

Ensure the pipe consumer **always reads data promptly** and never lets the pipe buffer fill up. If the consumer needs to pause processing, it should still drain the pipe (discarding data if necessary) to prevent the write side from blocking.

Alternatively, use a different audio backend (`alsa`, `pulseaudio`) that does not exhibit this issue.

## Upstream

This affects go-librespot as of June 2025. The issue is in:

- [`output/driver-pipe.go`](https://github.com/devgianlu/go-librespot/blob/master/output/driver-pipe.go)

No upstream fix exists at the time of writing.
