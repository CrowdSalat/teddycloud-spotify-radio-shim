package audio

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

const (
	defaultChunkSize = 4096
	defaultBufferLen = 8
)

// ChunkSource yields fixed-size PCM chunks for /stream consumers.
type ChunkSource interface {
	Chunks() <-chan []byte
}

// PulseRecorder reads PCM data from Source and emits fixed-size chunks on a
// buffered channel. The pump goroutine is started via Start and terminated via
// Stop or context cancellation.
type PulseRecorder struct {
	// Source is the PCM producer (s16le, 44100 Hz, stereo; 4 bytes/frame).
	// Zero value is nil and causes a panic if Start is called without one.
	Source io.Reader
	// ChunkSize is the number of bytes per emitted chunk. Must be a multiple
	// of 4 (one stereo frame). Zero uses the default.
	ChunkSize int
	// BufferLen is the buffered channel capacity. Zero uses the default.
	BufferLen int

	chunksSent atomic.Uint64
	dropped    atomic.Uint64

	mu      sync.Mutex
	chunkCh chan []byte
	stop    chan struct{}
	done    chan struct{}
	stopped bool
	start   sync.Once
}

func (p *PulseRecorder) chunkSize() int {
	if p.ChunkSize > 0 {
		return p.ChunkSize
	}

	return defaultChunkSize
}

func (p *PulseRecorder) bufferLen() int {
	if p.BufferLen > 0 {
		return p.BufferLen
	}

	return defaultBufferLen
}

// Chunks returns the buffered chunk channel. It is safe to call before Start;
// the channel is non-nil and buffered to the configured capacity.
func (p *PulseRecorder) Chunks() <-chan []byte {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.newChunkChan()
}

// Start lazily creates the channel and spawns the pump goroutine. It is safe
// to call multiple times; only the first call starts a pump.
func (p *PulseRecorder) Start(ctx context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()

	chunkCh := p.newChunkChan()
	p.start.Do(func() {
		p.stop = make(chan struct{})
		p.done = make(chan struct{})
		go p.pump(chunkCh, ctx)
	})
}

// Stop terminates the pump goroutine promptly. It is safe to call multiple
// times and before Start (no-op).
func (p *PulseRecorder) Stop() {
	p.mu.Lock()

	if p.stop == nil {
		p.stop = make(chan struct{})
	}

	if !p.stopped {
		close(p.stop)
		p.stopped = true
	}

	p.mu.Unlock()
}

// ChunksSent returns the number of chunks successfully sent to consumers.
func (p *PulseRecorder) ChunksSent() uint64 {
	return p.chunksSent.Load()
}

// Dropped returns the number of chunks dropped due to a full channel.
func (p *PulseRecorder) Dropped() uint64 {
	return p.dropped.Load()
}

func (p *PulseRecorder) pump(chunkCh chan []byte, ctx context.Context) {
	defer close(p.done)

	cs := p.chunkSize()
	buf := make([]byte, cs)
	var acc []byte

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stop:
			return
		default:
		}

		n, err := p.Source.Read(buf)
		if n > 0 {
			acc = append(acc, buf[:n]...)
		}

		for len(acc) >= cs {
			chunk := make([]byte, cs)
			copy(chunk, acc[:cs])

			select {
			case chunkCh <- chunk:
				p.chunksSent.Add(1)
			default:
				p.dropped.Add(1)
			}

			acc = acc[cs:]
		}

		if err != nil {
			if err != io.EOF {
				slog.Warn("recorder: read error", "err", err)
			}

			return
		}
	}
}

// newChunkChan returns the shared channel, creating it on first use. Callers
// must hold mu.
func (p *PulseRecorder) newChunkChan() chan []byte {
	if p.chunkCh == nil {
		p.chunkCh = make(chan []byte, p.bufferLen())
	}

	return p.chunkCh
}
