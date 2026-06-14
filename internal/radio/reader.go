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

	// ChunkBuf is the number of chunks buffered between the FIFO reader and
	// the HTTP writer. Small by design: backpressure from a slow HTTP consumer
	// propagates back through the channel to the FIFO, which in turn throttles
	// go-librespot to real-time. No audio is discarded in normal operation.
	ChunkBuf = 4
)

// ReaderMetrics exposes live counters for the reader goroutine.
type ReaderMetrics struct {
	BytesRead atomic.Uint64
}

// Reader reads raw PCM chunks from a pipe (go-librespot's FIFO output) and
// sends them to a caller-supplied channel via a blocking send. Backpressure
// from a slow consumer propagates naturally: channel full → reader blocks on
// send → FIFO fills → go-librespot throttles to real-time.
type Reader struct {
	source  io.Reader
	metrics ReaderMetrics
	logger  *slog.Logger
}

// NewReader creates a Reader backed by source.
func NewReader(source io.Reader, logger *slog.Logger) *Reader {
	return &Reader{source: source, logger: logger}
}

// Metrics returns a pointer to the reader's live metrics.
func (r *Reader) Metrics() *ReaderMetrics {
	return &r.metrics
}

// Run reads chunks from the source and sends them to out until the source
// returns EOF/error or ctx is cancelled. Sends are blocking — the caller
// controls the channel and the backpressure chain.
//
// The read buffer is pre-allocated once to avoid GC pressure in the hot path.
func (r *Reader) Run(ctx context.Context, out chan<- []byte) error {
	buf := make([]byte, ChunkSize)

	for {
		n, err := r.source.Read(buf)

		if n > 0 {
			r.metrics.BytesRead.Add(uint64(n))

			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			select {
			case out <- chunk: // blocks when consumer is slow — pure backpressure
			case <-ctx.Done():
				return ctx.Err()
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
