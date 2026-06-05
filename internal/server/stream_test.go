package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/audio"
	"github.com/janharings/teddycloud-spotify-radio-shim/internal/librespot"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// mockLibrespot starts a mock go-librespot API server and returns a client.
func mockLibrespot(t *testing.T, handler http.HandlerFunc) *librespot.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return librespot.New(srv.URL)
}

// okLibrespot returns a mock that always responds 200.
func okLibrespot(t *testing.T) *librespot.Client {
	t.Helper()
	return mockLibrespot(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// preloadedChan returns a closed channel pre-loaded with n chunks.
func preloadedChan(n, size int) chan []byte {
	ch := make(chan []byte, n)
	for i := 0; i < n; i++ {
		ch <- make([]byte, size)
	}
	close(ch)
	return ch
}

// TestStreamMissingURI: no spotify_uri → 400, play never called.
func TestStreamMissingURI(t *testing.T) {
	playCalled := false
	client := mockLibrespot(t, func(w http.ResponseWriter, r *http.Request) {
		playCalled = true
		w.WriteHeader(http.StatusOK)
	})

	h := NewStreamHandler(client, make(chan []byte), testLogger())
	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if playCalled {
		t.Error("play should not be called without a URI")
	}
}

// TestStreamPlayError: go-librespot returns 500 → 502 to client.
func TestStreamPlayError(t *testing.T) {
	client := mockLibrespot(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	h := NewStreamHandler(client, make(chan []byte), testLogger())
	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:track:abc", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// TestStreamContentTypeAndWAVHeader: 200 response has correct Content-Type
// and a valid WAV header with streaming sentinel sizes.
func TestStreamContentTypeAndWAVHeader(t *testing.T) {
	h := NewStreamHandler(okLibrespot(t), preloadedChan(2, audio.ChunkSize), testLogger())

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:track:abc", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != audio.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, audio.ContentType)
	}

	body := rec.Body.Bytes()

	// RIFF marker.
	if !bytes.HasPrefix(body, []byte("RIFF")) {
		t.Fatalf("body does not start with RIFF: %q", body[:min(len(body), 8)])
	}
	// Streaming size sentinel.
	if got := binary.LittleEndian.Uint32(body[4:8]); got != 0xFFFFFFFF {
		t.Errorf("RIFF size = %#x, want 0xFFFFFFFF", got)
	}
	// WAVE marker.
	if string(body[8:12]) != "WAVE" {
		t.Errorf("expected WAVE at byte 8, got %q", body[8:12])
	}
	// WAV header is 44 bytes; remaining bytes are audio payload.
	const wavHeader = 44
	wantPayload := 2 * audio.ChunkSize
	if got := len(body) - wavHeader; got != wantPayload {
		t.Errorf("payload = %d bytes, want %d", got, wantPayload)
	}
}

// TestStreamWAVHeaderSize: WAV header is exactly 44 bytes.
func TestStreamWAVHeaderSize(t *testing.T) {
	var buf bytes.Buffer
	if err := audio.WriteStreamingWAVHeader(&buf); err != nil {
		t.Fatalf("WriteStreamingWAVHeader: %v", err)
	}
	if n := buf.Len(); n != 44 {
		t.Errorf("WAV header = %d bytes, want 44", n)
	}
}

// TestStreamClientDisconnect: cancelling the request context causes the
// handler to exit promptly without blocking.
func TestStreamClientDisconnect(t *testing.T) {
	h := NewStreamHandler(okLibrespot(t), make(chan []byte), testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:track:abc", nil)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after context cancellation")
	}
}

// TestStreamChannelClose: handler exits when the audio channel is closed.
func TestStreamChannelClose(t *testing.T) {
	ch := make(chan []byte)
	close(ch)

	h := NewStreamHandler(okLibrespot(t), ch, testLogger())
	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:track:abc", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit when channel was closed")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
