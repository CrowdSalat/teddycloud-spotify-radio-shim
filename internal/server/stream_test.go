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

	"github.com/gorilla/websocket"
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
	return librespot.NewClient(srv.URL)
}

// okLibrespot returns a mock that always responds 200.
func okLibrespot(t *testing.T) *librespot.Client {
	t.Helper()
	return mockLibrespot(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// mockEvents returns an Events client backed by a mock WebSocket server
// and a send function to inject synthetic events.
func mockEvents(t *testing.T) (*librespot.EventStream, func(librespot.Event)) {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conns := make(chan *websocket.Conn, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		conns <- conn
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)

	evts := librespot.NewEventStream(srv.URL, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go evts.Run(ctx)

	// Wait for WebSocket connection to be established.
	var activeConn *websocket.Conn
	select {
	case activeConn = <-conns:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("events WebSocket did not connect in time")
	}

	sendFn := func(ev librespot.Event) {
		msg, err := ev.MarshalWire()
		if err != nil {
			t.Errorf("MarshalWire: %v", err)
			return
		}
		if err := activeConn.WriteMessage(websocket.TextMessage, msg); err != nil {
			t.Logf("send event: %v", err)
		}
	}

	return evts, sendFn
}

// TestStreamMissingURI: no spotify_uri → 400, play never called.
func TestStreamMissingURI(t *testing.T) {
	playCalled := false
	client := mockLibrespot(t, func(w http.ResponseWriter, r *http.Request) {
		playCalled = true
		w.WriteHeader(http.StatusOK)
	})
	evts, _ := mockEvents(t)

	h := NewStreamHandler(client, evts, make(chan []byte), testLogger())
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
	evts, _ := mockEvents(t)

	h := NewStreamHandler(client, evts, make(chan []byte), testLogger())
	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:track:abc", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
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

// TestStreamContentTypeAndWAVHeader: 200 response has correct Content-Type,
// a valid WAV header, and audio bytes. Audio is sent AFTER will_play fires
// (simulating the real track-switch flow where flushChannel discards old data
// and new data arrives from the reader).
func TestStreamContentTypeAndWAVHeader(t *testing.T) {
	const uri = "spotify:track:abc"
	evts, send := mockEvents(t)
	ch := make(chan []byte, audio.ChannelCapacity)

	h := NewStreamHandler(okLibrespot(t), evts, ch, testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri, nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(rec, req)
	}()

	// Send will_play: handler exits waitWillPlay, calls flushChannel (empty),
	// then enters audio loop.
	send(librespot.Event{Type: librespot.EventWillPlay, URI: uri})
	time.Sleep(30 * time.Millisecond) // let handler reach audio loop

	// Feed two chunks after will_play (simulates new track PCM arriving).
	ch <- make([]byte, audio.ChunkSize)
	ch <- make([]byte, audio.ChunkSize)
	time.Sleep(30 * time.Millisecond) // let handler forward them

	cancel()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != audio.ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, audio.ContentType)
	}

	body := rec.Body.Bytes()
	if !bytes.HasPrefix(body, []byte("RIFF")) {
		t.Fatalf("body does not start with RIFF: %q", body[:min(len(body), 8)])
	}
	if got := binary.LittleEndian.Uint32(body[4:8]); got != 0xFFFFFFFF {
		t.Errorf("RIFF size = %#x, want 0xFFFFFFFF", got)
	}
	if string(body[8:12]) != "WAVE" {
		t.Errorf("expected WAVE at byte 8, got %q", body[8:12])
	}

	const wavHeader = 44
	wantPayload := 2 * audio.ChunkSize
	if got := len(body) - wavHeader; got != wantPayload {
		t.Errorf("payload = %d bytes, want %d", got, wantPayload)
	}
}

// TestStreamClientDisconnect: cancelling the request context causes the
// handler to exit promptly without blocking.
func TestStreamClientDisconnect(t *testing.T) {
	const uri = "spotify:track:abc"
	evts, send := mockEvents(t)

	h := NewStreamHandler(okLibrespot(t), evts, make(chan []byte), testLogger())

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri, nil)
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// Send will_play then cancel so the handler is in the audio loop when cancelled.
	send(librespot.Event{Type: librespot.EventWillPlay, URI: uri})
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after context cancellation")
	}
}

// TestStreamChannelClose: handler exits when the audio channel is closed.
func TestStreamChannelClose(t *testing.T) {
	const uri = "spotify:track:abc"
	evts, send := mockEvents(t)

	ch := make(chan []byte)
	close(ch)

	h := NewStreamHandler(okLibrespot(t), evts, ch, testLogger())
	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri, nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// Send will_play: handler flushes the closed channel (ok=false → returns),
	// exits immediately.
	send(librespot.Event{Type: librespot.EventWillPlay, URI: uri})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit when channel was closed")
	}
}

// TestStreamHotSwap: a second /stream request cancels the first.
func TestStreamHotSwap(t *testing.T) {
	const uri1 = "spotify:track:first"
	const uri2 = "spotify:track:second"

	evts, send := mockEvents(t)
	ch := make(chan []byte, audio.ChannelCapacity)

	h := NewStreamHandler(okLibrespot(t), evts, ch, testLogger())

	// Start first stream.
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	req1 := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri1, nil)
	req1 = req1.WithContext(ctx1)

	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		h.ServeHTTP(httptest.NewRecorder(), req1)
	}()

	// Let first stream get into the audio loop.
	send(librespot.Event{Type: librespot.EventWillPlay, URI: uri1})
	time.Sleep(30 * time.Millisecond)

	// Start second stream — should cancel the first.
	req2 := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri2, nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	req2 = req2.WithContext(ctx2)

	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		h.ServeHTTP(httptest.NewRecorder(), req2)
	}()

	// First stream should exit promptly.
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("first stream did not exit when second stream started")
	}

	// Second stream is running — send will_play and cancel it.
	send(librespot.Event{Type: librespot.EventWillPlay, URI: uri2})
	time.Sleep(20 * time.Millisecond)
	cancel2()

	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("second stream did not exit after context cancellation")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
