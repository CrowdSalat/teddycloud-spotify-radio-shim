package radio

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/janharings/teddycloud-spotify-radio-shim/internal/librespot"
)

// mockLibrespot starts a test HTTP server simulating go-librespot's API.
func mockLibrespot(t *testing.T, handler http.HandlerFunc) *librespot.Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return librespot.NewClient(srv.URL)
}

// okClient returns a client where every API call succeeds.
func okClient(t *testing.T) *librespot.Client {
	t.Helper()
	return mockLibrespot(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestCoordinatorMissingURI: no spotify_uri → 400, API never called.
func TestCoordinatorMissingURI(t *testing.T) {
	called := false
	client := mockLibrespot(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	c := NewPlaybackCoordinator(client, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/stream", nil)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if called {
		t.Error("API should not be called without a URI")
	}
}

// TestCoordinatorPlayError: play returns 500 → coordinator returns 502.
func TestCoordinatorPlayError(t *testing.T) {
	client := mockLibrespot(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	c := NewPlaybackCoordinator(client, testLogger())

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri=spotify:track:abc", nil)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// TestCoordinatorStreamsAudio: correct headers and audio bytes flow from
// the chunk channel to the HTTP response.
func TestCoordinatorStreamsAudio(t *testing.T) {
	const uri = "spotify:track:abc"
	c := NewPlaybackCoordinator(okClient(t), testLogger())

	// Feed 3 chunks then close so the handler exits naturally via channel close.
	go func() {
		for i := 0; i < 3; i++ {
			c.chunks <- make([]byte, ChunkSize)
		}
		close(c.chunks)
	}()

	req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri, nil)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ContentType {
		t.Errorf("Content-Type = %q, want %q", ct, ContentType)
	}

	const wavHeader = 44
	wantBody := wavHeader + 3*ChunkSize
	if got := rec.Body.Len(); got != wantBody {
		t.Errorf("body = %d bytes, want %d (44 WAV header + 3 chunks)", got, wantBody)
	}
}

// TestCoordinatorSameURIResumes: a second request with the same URI does
// not call the play API — it just resumes reading from the channel.
func TestCoordinatorSameURIResumes(t *testing.T) {
	const uri = "spotify:track:abc"
	playCalls := 0
	client := mockLibrespot(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/player/play" {
			playCalls++
		}
		w.WriteHeader(http.StatusOK)
	})
	c := NewPlaybackCoordinator(client, testLogger())

	serve := func() {
		go func() {
			c.chunks <- make([]byte, ChunkSize)
			close(c.chunks)
		}()
		req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri, nil)
		c.ServeHTTP(httptest.NewRecorder(), req)
	}

	serve() // first request — play is called, lastURI set

	// Re-open channel for second request.
	c.chunks = make(chan []byte, ChunkBuf)
	serve() // second request, same URI — play must NOT be called again

	if playCalls != 1 {
		t.Errorf("play called %d times, want exactly 1 (resume should skip play)", playCalls)
	}
}

// TestCoordinatorHotSwap: a new URI pauses the current stream and plays
// the new one. The first stream's goroutine exits promptly.
func TestCoordinatorHotSwap(t *testing.T) {
	const uri1 = "spotify:track:first"
	const uri2 = "spotify:track:second"

	paths := make(chan string, 10)
	client := mockLibrespot(t, func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	c := NewPlaybackCoordinator(client, testLogger())

	// First stream: feed one chunk, then block so the handler stays alive.
	// The hot-swap (second request) will cancel its context.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri1, nil)
		c.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// Let first stream get into the audio loop (play called, header sent).
	time.Sleep(50 * time.Millisecond)

	// Second stream: different URI, triggers hot-swap.
	// Feed one chunk to drain the pauseWithDrain loop, then close.
	go func() {
		time.Sleep(20 * time.Millisecond)
		c.chunks <- make([]byte, ChunkSize) // unblocks pauseWithDrain drain
		time.Sleep(20 * time.Millisecond)
		close(c.chunks)
	}()

	req2 := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri2, nil)
	c.ServeHTTP(httptest.NewRecorder(), req2)

	// First handler must have exited.
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first stream did not exit after hot-swap")
	}

	// Collect API calls: expect play(uri1), pause, play(uri2).
	close(paths)
	var calls []string
	for p := range paths {
		calls = append(calls, p)
	}

	if len(calls) < 3 {
		t.Fatalf("expected at least 3 API calls, got %v", calls)
	}
}

// TestCoordinatorClientDisconnect: closing the chunk channel exits the handler.
func TestCoordinatorClientDisconnect(t *testing.T) {
	const uri = "spotify:track:abc"
	c := NewPlaybackCoordinator(okClient(t), testLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "/stream?spotify_uri="+uri, nil)
		c.ServeHTTP(httptest.NewRecorder(), req)
	}()

	// Handler is blocked in the play+stream path. Close chunks to signal EOF.
	time.Sleep(30 * time.Millisecond)
	close(c.chunks)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not exit after channel close")
	}
}
