package audio

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"
)

// sourceFunc adapts a function into an io.Reader.
type sourceFunc func(p []byte) (int, error)

func (f sourceFunc) Read(p []byte) (int, error) { return f(p) }

// waitChunks reads n chunks from ch within timeout, failing t otherwise.
func waitChunks(t *testing.T, ch <-chan []byte, n int, timeout time.Duration) [][]byte {
	t.Helper()

	var chunks [][]byte

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for len(chunks) < n {
		select {
		case c := <-ch:
			chunks = append(chunks, c)
		case <-timer.C:
			t.Fatalf("waitChunks: got %d/%d chunks within %v", len(chunks), n, timeout)
		}
	}

	return chunks
}

// waitDone blocks until the done channel is closed or timeout elapses.
func waitDone(t *testing.T, done <-chan struct{}, timeout time.Duration) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("pump did not terminate within timeout")
	}
}

func TestPulseRecorderDeliversChunks(t *testing.T) {
	const chunkSize = 16
	wantN := 5

	var mu sync.Mutex
	reads := [][]byte{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		{16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31},
		{32, 33, 34, 35, 36, 37, 38, 39, 40, 41, 42, 43, 44, 45, 46, 47},
		{48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59, 60, 61, 62, 63},
		{64, 65, 66, 67, 68, 69, 70, 71, 72, 73, 74, 75, 76, 77, 78, 79},
	}
	idx := 0

	src := sourceFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()

		if idx >= len(reads) {
			return 0, io.EOF
		}

		n := copy(p, reads[idx])
		idx++

		return n, nil
	})

	r := &PulseRecorder{Source: src, ChunkSize: chunkSize, BufferLen: wantN}
	ctx := context.Background()
	r.Start(ctx)
	defer r.Stop()

	chunks := waitChunks(t, r.Chunks(), wantN, 2*time.Second)

	for i, c := range chunks {
		if len(c) != chunkSize {
			t.Fatalf("chunk %d: got len %d, want %d", i, len(c), chunkSize)
		}

		want := byte(i * chunkSize)
		if c[0] != want {
			t.Fatalf("chunk %d: first byte = %d, want %d", i, c[0], want)
		}
	}

	if got := r.ChunksSent(); got != uint64(wantN) {
		t.Errorf("ChunksSent() = %d, want %d", got, wantN)
	}

	if got := r.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0", got)
	}
}

func TestPulseRecorderDiscardsWhenFull(t *testing.T) {
	const chunkSize = 16
	const totalChunks = 64

	data := make([]byte, chunkSize*totalChunks)
	for i := range data {
		data[i] = byte(i)
	}

	var mu sync.Mutex
	off := 0

	src := sourceFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()

		if off >= len(data) {
			return 0, io.EOF
		}

		n := copy(p, data[off:])
		off += n

		return n, nil
	})

	r := &PulseRecorder{Source: src, ChunkSize: chunkSize, BufferLen: 1}
	ctx := context.Background()
	r.Start(ctx)

	waitDone(t, r.done, 2*time.Second)

	if got := r.Dropped(); got == 0 {
		t.Errorf("Dropped() = 0, want >= 1")
	}

	mu.Lock()
	consumed := off
	mu.Unlock()

	if consumed != len(data) {
		t.Errorf("source consumed %d/%d bytes (pump stalled?)", consumed, len(data))
	}
}

func TestPulseRecorderChunksAlwaysFixedSize(t *testing.T) {
	cases := []struct {
		name      string
		chunkSize int
		fragments [][]byte
	}{
		{
			name:      "small_fragments",
			chunkSize: 16,
			fragments: [][]byte{
				{0, 1, 2},
				{3, 4, 5, 6, 7, 8, 9},
				{10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21},
				{22, 23, 24, 25, 26, 27, 28, 29, 30, 31},
			},
		},
		{
			name:      "default_chunk_size",
			chunkSize: 4096,
			fragments: func() [][]byte {
				// Awkward fragment sizes that sum to a multiple of 4096.
				frags := [][]byte{}
				remaining := 4096
				sizes := []int{3, 9, 22, 5, 127, 4096 - 3 - 9 - 22 - 5 - 127}
				for _, s := range sizes {
					chunk := make([]byte, s)
					for i := range chunk {
						chunk[i] = byte(i)
					}
					frags = append(frags, chunk)
					remaining -= s
				}

				if remaining != 0 {
					t := make([]byte, remaining)
					frags = append(frags, t)
				}

				return frags
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			frags := tc.fragments
			idx := 0

			src := sourceFunc(func(p []byte) (int, error) {
				mu.Lock()
				defer mu.Unlock()

				if idx >= len(frags) {
					return 0, io.EOF
				}

				n := copy(p, frags[idx])
				idx++

				return n, nil
			})

			r := &PulseRecorder{Source: src, ChunkSize: tc.chunkSize, BufferLen: 4}
			r.Start(context.Background())
			defer r.Stop()

			// Read chunks until we've consumed all fragments; count how many
			// full chunks were emitted. All must be exactly chunkSize.
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()

			got := 0
			totalBytes := 0
			for _, f := range frags {
				totalBytes += len(f)
			}

			wantChunks := totalBytes / tc.chunkSize

			for {
				select {
				case c := <-r.Chunks():
					if len(c) != tc.chunkSize {
						t.Fatalf("chunk %d: got len %d, want %d", got, len(c), tc.chunkSize)
					}

					got++

					if got >= wantChunks {
						return
					}
				case <-timer.C:
					t.Fatalf("got %d/%d chunks within timeout", got, wantChunks)
				}
			}
		})
	}
}

// TestPulseRecorderDone verifies that Done returns a nil channel before Start
// and a closed channel shortly after the pump exits (here via immediate EOF).
func TestPulseRecorderDone(t *testing.T) {
	r := &PulseRecorder{Source: sourceFunc(func([]byte) (int, error) {
		return 0, io.EOF
	}), ChunkSize: 16, BufferLen: 1}

	if got := r.Done(); got != nil {
		t.Fatalf("Done() before Start = %v, want nil", got)
	}

	r.Start(context.Background())
	defer r.Stop()

	waitDone(t, r.Done(), time.Second)
}

func TestPulseRecorderStopTerminatesPump(t *testing.T) {
	block := make(chan struct{})

	src := sourceFunc(func(p []byte) (int, error) {
		<-block

		return 0, io.ErrClosedPipe
	})

	r := &PulseRecorder{Source: src, ChunkSize: 16, BufferLen: 4}
	r.Start(context.Background())

	time.Sleep(50 * time.Millisecond)
	r.Stop()
	close(block)

	waitDone(t, r.done, time.Second)

	if got := r.ChunksSent(); got != 0 {
		t.Errorf("ChunksSent() = %d, want 0", got)
	}
}

func TestPulseRecorderContextCancelTerminatesPump(t *testing.T) {
	block := make(chan struct{})

	src := sourceFunc(func(p []byte) (int, error) {
		<-block

		return 0, io.ErrClosedPipe
	})

	r := &PulseRecorder{Source: src, ChunkSize: 16, BufferLen: 4}
	ctx, cancel := context.WithCancel(context.Background())
	r.Start(ctx)

	time.Sleep(50 * time.Millisecond)
	cancel()
	close(block)

	waitDone(t, r.done, time.Second)

	if got := r.ChunksSent(); got != 0 {
		t.Errorf("ChunksSent() = %d, want 0", got)
	}
}

func TestPulseRecorderDefaults(t *testing.T) {
	r := &PulseRecorder{}

	if r.chunkSize() != defaultChunkSize {
		t.Errorf("chunkSize() = %d, want %d", r.chunkSize(), defaultChunkSize)
	}

	if r.bufferLen() != defaultBufferLen {
		t.Errorf("bufferLen() = %d, want %d", r.bufferLen(), defaultBufferLen)
	}

	ch := r.Chunks()
	if ch == nil {
		t.Fatal("Chunks() returned nil channel")
	}
}

// TestPulseRecorderLifecycleGuards exercises the concurrency-correct
// idempotency of Start/Stop: Stop before Start, double Stop, double Start with
// diverging contexts, and Stop without ever starting all must not panic and
// must terminate any spawned pump.
func TestPulseRecorderLifecycleGuards(t *testing.T) {
	t.Run("stop_before_start", func(t *testing.T) {
		r := &PulseRecorder{ChunkSize: 16, BufferLen: 1}
		r.Stop()
		r.Stop()
	})

	t.Run("double_stop_running", func(t *testing.T) {
		block := make(chan struct{})
		src := sourceFunc(func(p []byte) (int, error) {
			<-block

			return 0, io.EOF
		})

		r := &PulseRecorder{Source: src, ChunkSize: 16, BufferLen: 1}
		r.Start(context.Background())
		r.Stop()
		r.Stop()
		close(block)

		waitDone(t, r.done, time.Second)
	})

	t.Run("double_start", func(t *testing.T) {
		entered := make(chan struct{})
		block := make(chan struct{})
		var n int
		var mu sync.Mutex

		src := sourceFunc(func(p []byte) (int, error) {
			mu.Lock()
			n++
			mu.Unlock()

			select {
			case <-entered:
			default:
				close(entered)
			}

			<-block

			return 0, io.EOF
		})

		r := &PulseRecorder{Source: src, ChunkSize: 16, BufferLen: 1}
		r.Start(context.Background())
		r.Start(context.Background())

		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("pump never entered source read")
		}

		r.Stop()
		close(block)

		waitDone(t, r.done, time.Second)

		mu.Lock()
		defer mu.Unlock()

		if n != 1 {
			t.Errorf("source read %d times, want 1 (only one pump should run)", n)
		}
	})
}
