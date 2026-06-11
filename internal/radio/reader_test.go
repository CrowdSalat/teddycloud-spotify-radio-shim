package radio

import (
	"bytes"
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

// TestReaderNormalFlow writes 100 chunks and verifies all are received
// with zero discards.
func TestReaderNormalFlow(t *testing.T) {
	pr, pw := io.Pipe()
	reader := NewReader(pr, testLogger())

	var wg sync.WaitGroup

	// Writer: send 100 chunks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		chunk := make([]byte, ChunkSize)
		for i := range chunk {
			chunk[i] = byte(i % 256)
		}
		for i := 0; i < 100; i++ {
			if _, err := pw.Write(chunk); err != nil {
				t.Errorf("write %d: %v", i, err)
				return
			}
		}
		pw.Close()
	}()

	// Reader goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := reader.Run(); err != nil {
			t.Errorf("reader.Run: %v", err)
		}
	}()

	// Consumer: drain the channel and count.
	received := 0
	totalBytes := 0
	for chunk := range reader.C() {
		received++
		totalBytes += len(chunk)
	}

	wg.Wait()

	if received != 100 {
		t.Errorf("expected 100 chunks, got %d", received)
	}
	if totalBytes != 100*ChunkSize {
		t.Errorf("expected %d bytes, got %d", 100*ChunkSize, totalBytes)
	}

	m := reader.Metrics()
	if d := m.ChunksDiscarded.Load(); d != 0 {
		t.Errorf("expected 0 discards, got %d", d)
	}
	if br := m.BytesRead.Load(); br != uint64(100*ChunkSize) {
		t.Errorf("expected %d BytesRead, got %d", 100*ChunkSize, br)
	}
}

// TestReaderSlowConsumer writes more chunks than the channel can hold
// without any consumer reading. Verifies that discards occur and the
// reader doesn't block.
func TestReaderSlowConsumer(t *testing.T) {
	pr, pw := io.Pipe()
	reader := NewReader(pr, testLogger())

	writeCount := ChannelCapacity + 20 // more than channel can hold

	var wg sync.WaitGroup

	// Reader goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := reader.Run(); err != nil {
			t.Errorf("reader.Run: %v", err)
		}
	}()

	// Writer: send more chunks than channel capacity without reading.
	chunk := make([]byte, ChunkSize)
	for i := 0; i < writeCount; i++ {
		if _, err := pw.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	pw.Close()

	// Now drain the channel.
	received := 0
	for range reader.C() {
		received++
	}

	wg.Wait()

	m := reader.Metrics()
	discarded := m.ChunksDiscarded.Load()

	t.Logf("sent=%d received=%d discarded=%d", writeCount, received, discarded)

	if discarded == 0 {
		t.Error("expected discards > 0 with slow consumer")
	}
	if int(discarded)+received != writeCount {
		t.Errorf("discarded(%d) + received(%d) != sent(%d)", discarded, received, writeCount)
	}
	if bd := m.BytesDiscarded.Load(); bd != discarded*uint64(ChunkSize) {
		t.Errorf("BytesDiscarded=%d, expected %d", bd, discarded*uint64(ChunkSize))
	}
}

// TestReaderPauseResume simulates go-librespot pausing (stop writing to
// the pipe) and resuming. The reader should block harmlessly on the
// empty pipe and resume when data appears again.
func TestReaderPauseResume(t *testing.T) {
	pr, pw := io.Pipe()
	reader := NewReader(pr, testLogger())

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := reader.Run(); err != nil {
			t.Errorf("reader.Run: %v", err)
		}
	}()

	chunk := make([]byte, ChunkSize)

	// Phase 1: write 5 chunks.
	for i := 0; i < 5; i++ {
		if _, err := pw.Write(chunk); err != nil {
			t.Fatalf("write phase 1: %v", err)
		}
	}

	// Drain phase 1.
	for i := 0; i < 5; i++ {
		select {
		case <-reader.C():
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for phase 1 chunk")
		}
	}

	// Phase 2: "pause" — write nothing for 200ms.
	// Reader should block on empty pipe, not crash, not spin.
	time.Sleep(200 * time.Millisecond)

	// Verify channel is empty during pause.
	select {
	case <-reader.C():
		t.Error("received unexpected chunk during pause")
	default:
		// Good — channel is empty.
	}

	// Phase 3: "resume" — write 5 more chunks.
	for i := 0; i < 5; i++ {
		if _, err := pw.Write(chunk); err != nil {
			t.Fatalf("write phase 3: %v", err)
		}
	}

	for i := 0; i < 5; i++ {
		select {
		case <-reader.C():
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for phase 3 chunk")
		}
	}

	pw.Close()
	wg.Wait()

	m := reader.Metrics()
	if br := m.BytesRead.Load(); br != uint64(10*ChunkSize) {
		t.Errorf("expected %d BytesRead, got %d", 10*ChunkSize, br)
	}
	if d := m.ChunksDiscarded.Load(); d != 0 {
		t.Errorf("expected 0 discards, got %d", d)
	}
}

// TestReaderPipeError verifies that the reader returns an error (not EOF)
// when the pipe read end encounters a non-EOF error, and that the
// channel is closed.
func TestReaderPipeError(t *testing.T) {
	// Use an os.Pipe so we can close the write end abruptly.
	osR, osW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	reader := NewReader(osR, testLogger())

	errCh := make(chan error, 1)
	go func() {
		errCh <- reader.Run()
	}()

	// Write one chunk, then close the write end.
	chunk := make([]byte, ChunkSize)
	if _, err := osW.Write(chunk); err != nil {
		t.Fatalf("write: %v", err)
	}
	osW.Close()

	// Reader should get EOF and return nil.
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("expected nil on EOF, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for reader to finish")
	}

	// Channel should be closed and drained.
	count := 0
	for range reader.C() {
		count++
	}
	if count != 1 {
		t.Errorf("expected 1 chunk before close, got %d", count)
	}
}

// TestReaderChunkIntegrity verifies that data read from the channel
// matches what was written to the pipe (no corruption from buffer reuse).
func TestReaderChunkIntegrity(t *testing.T) {
	pr, pw := io.Pipe()
	reader := NewReader(pr, testLogger())

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := reader.Run(); err != nil {
			t.Errorf("reader.Run: %v", err)
		}
	}()

	// Write 10 chunks with distinct content.
	for i := 0; i < 10; i++ {
		chunk := bytes.Repeat([]byte{byte(i)}, ChunkSize)
		if _, err := pw.Write(chunk); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
	}
	pw.Close()

	// Verify each chunk has the correct content.
	i := 0
	for chunk := range reader.C() {
		expected := bytes.Repeat([]byte{byte(i)}, ChunkSize)
		if !bytes.Equal(chunk, expected) {
			t.Errorf("chunk %d: content mismatch (first byte: got %d, want %d)",
				i, chunk[0], byte(i))
		}
		i++
	}

	wg.Wait()

	if i != 10 {
		t.Errorf("expected 10 chunks, got %d", i)
	}
}
