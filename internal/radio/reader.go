// Package radio provides the FIFO reader that bridges go-librespot's pipe
// output to the shim's HTTP streaming layer.
package radio

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
)

const (
	// ChunkSize matches go-librespot's pipe write size:
	// 4096 float32 samples → 8192 bytes at s16le.
	ChunkSize = 8192

	// ChannelCapacity is the number of chunks the buffered channel holds.
	// 32 × 8 KB = 256 KB ≈ 1.5 seconds of audio at 44.1 kHz stereo s16le.
	ChannelCapacity = 32
)

// ReaderMetrics exposes counters for monitoring the reader goroutine.
type ReaderMetrics struct {
	BytesRead       atomic.Uint64
	BytesDiscarded  atomic.Uint64
	ChunksDiscarded atomic.Uint64
}

// Reader continuously reads from a pipe and sends chunks to a buffered
// channel. If the channel is full (downstream stall), it discards data
// to prevent the FIFO from backing up and deadlocking go-librespot.
//
// See ADR-005 for the invariant: never stop reading for >~300ms.
type Reader struct {
	source  io.Reader
	out     chan []byte
	metrics ReaderMetrics
	logger  *slog.Logger
}

// NewReader creates a Reader that reads from source and sends chunks to
// an internal buffered channel. Retrieve the channel with C().
func NewReader(source io.Reader, logger *slog.Logger) *Reader {
	return &Reader{
		source: source,
		out:    make(chan []byte, ChannelCapacity),
		logger: logger,
	}
}

// C returns the read-only channel that consumers pull audio chunks from.
func (r *Reader) C() <-chan []byte {
	return r.out
}

// Metrics returns a pointer to the reader's live metrics.
func (r *Reader) Metrics() *ReaderMetrics {
	return &r.metrics
}

// Run reads from the source in a loop until the source returns an error
// (typically io.EOF or a broken pipe). It blocks and is intended to be
// called as a goroutine.
//
// The read buffer is pre-allocated once to avoid GC pressure in the
// hot path.
func (r *Reader) Run() error {
	return r.pump(r.out, true)
}

// RunInto is like Run but writes into an external channel instead of
// the Reader's own internal channel. The channel is NOT closed when
// the source EOF's — the caller owns the channel lifetime. Use this
// when the channel must outlive a single FIFO session (e.g. across
// go-librespot restarts).
func (r *Reader) RunInto(ctx context.Context, out chan<- []byte) error {
	buf := make([]byte, ChunkSize)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		n, err := r.source.Read(buf)

		if n > 0 {
			r.metrics.BytesRead.Add(uint64(n))
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			select {
			case out <- chunk:
			default:
				r.metrics.BytesDiscarded.Add(uint64(n))
				r.metrics.ChunksDiscarded.Add(1)
			}
		}

		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// pump is the shared read loop. If closeOnDone is true the channel is
// closed when the source returns EOF (used by Run for tests).
func (r *Reader) pump(out chan []byte, closeOnDone bool) error {
	buf := make([]byte, ChunkSize)

	for {
		n, err := r.source.Read(buf)

		if n > 0 {
			r.metrics.BytesRead.Add(uint64(n))

			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			select {
			case out <- chunk:
			default:
				r.metrics.BytesDiscarded.Add(uint64(n))
				r.metrics.ChunksDiscarded.Add(1)
			}
		}

		if err != nil {
			if closeOnDone {
				close(out)
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}
