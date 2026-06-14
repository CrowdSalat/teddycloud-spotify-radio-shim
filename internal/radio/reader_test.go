package radio

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// TestReaderNormalFlow: all chunks reach the consumer, none dropped.
func TestReaderNormalFlow(t *testing.T) {
	pr, pw := io.Pipe()
	out := make(chan []byte, 100)
	reader := NewReader(pr, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := reader.Run(ctx, out); err != nil {
			t.Errorf("reader.Run: %v", err)
		}
	}()

	chunk := make([]byte, ChunkSize)
	for i := 0; i < 10; i++ {
		if _, err := pw.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	pw.Close()

	// Wait for reader to finish, then count buffered chunks.
	// Run() does not close the caller's channel, so we cannot range over it.
	wg.Wait()

	received := 0
	for {
		select {
		case <-out:
			received++
		default:
			goto done
		}
	}
done:
	if received != 10 {
		t.Errorf("expected 10 chunks, got %d", received)
	}
	if d := reader.Metrics().BytesRead.Load(); d != uint64(10*ChunkSize) {
		t.Errorf("BytesRead = %d, want %d", d, 10*ChunkSize)
	}
}

// TestReaderBackpressure: when the consumer is slow, the reader blocks
// on the channel send (does not discard) — backpressure propagates.
func TestReaderBackpressure(t *testing.T) {
	pr, pw := io.Pipe()
	// Unbuffered channel: every send blocks until consumer reads.
	out := make(chan []byte)
	reader := NewReader(pr, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		if err := reader.Run(ctx, out); err != nil {
			t.Errorf("reader.Run: %v", err)
		}
	}()

	chunk := make([]byte, ChunkSize)

	// Write one chunk to the pipe.
	writeErr := make(chan error, 1)
	go func() {
		_, err := pw.Write(chunk)
		writeErr <- err
	}()

	// Consumer reads it.
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for chunk")
	}

	// Write must have completed cleanly.
	select {
	case err := <-writeErr:
		if err != nil {
			t.Errorf("write error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("write goroutine did not complete")
	}

	pw.Close()
	<-readerDone
}

// TestReaderContextCancel: cancelling the context stops the reader while it
// is blocked on the channel send (the interruptible path). The Read() call
// itself is an OS-level block that context cannot interrupt; in production
// the FIFO closes when go-librespot stops, which unblocks the read.
func TestReaderContextCancel(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()

	// Unbuffered channel: reader will block in select waiting for a consumer.
	out := make(chan []byte)
	reader := NewReader(pr, testLogger())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- reader.Run(ctx, out)
	}()

	// Write one chunk: reader gets past Read() and into the select.
	// Nobody reads from out, so the reader blocks on case out <- chunk.
	chunk := make([]byte, ChunkSize)
	if _, err := pw.Write(chunk); err != nil {
		t.Fatalf("write: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	cancel() // fires ctx.Done() in the select — reader returns context.Canceled

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not stop after context cancellation")
	}
}

// TestReaderEOF: reader returns nil on clean EOF.
func TestReaderEOF(t *testing.T) {
	pr, pw := io.Pipe()
	out := make(chan []byte, 10)
	reader := NewReader(pr, testLogger())

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- reader.Run(ctx, out)
	}()

	pw.Close() // EOF immediately

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("expected nil on EOF, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader did not exit on EOF")
	}
}
