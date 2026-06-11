package librespot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// mockServer starts a test HTTP server and returns the client pointing at it.
func mockServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL)
}

func TestPlay(t *testing.T) {
	var gotBody string
	var gotMethod string

	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.WriteHeader(http.StatusOK)
	})

	err := client.Play(context.Background(), "spotify:playlist:abc123")
	if err != nil {
		t.Fatalf("Play returned error: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if want := `{"uri":"spotify:playlist:abc123"}`; gotBody != want {
		t.Errorf("body = %q, want %q", gotBody, want)
	}
}

func TestPlayError(t *testing.T) {
	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	err := client.Play(context.Background(), "spotify:playlist:abc123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestPauseResumeNextPrev(t *testing.T) {
	calls := map[string]int{}

	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls[r.URL.Path]++
		w.WriteHeader(http.StatusOK)
	})

	ctx := context.Background()

	for _, tc := range []struct {
		fn   func() error
		path string
	}{
		{func() error { return client.Pause(ctx) }, "/player/pause"},
		{func() error { return client.Resume(ctx) }, "/player/resume"},
		{func() error { return client.Next(ctx) }, "/player/next"},
		{func() error { return client.Prev(ctx) }, "/player/prev"},
	} {
		if err := tc.fn(); err != nil {
			t.Errorf("%s returned error: %v", tc.path, err)
		}
		if calls[tc.path] != 1 {
			t.Errorf("%s: expected 1 call, got %d", tc.path, calls[tc.path])
		}
	}
}

func TestStatus(t *testing.T) {
	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		//nolint:errcheck
		w.Write([]byte(`{"username":"testuser","stopped":false,"paused":true}`))
	})

	s, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if s.Username != "testuser" {
		t.Errorf("Username = %q, want %q", s.Username, "testuser")
	}
	if !s.Paused {
		t.Error("expected Paused=true")
	}
	if s.Stopped {
		t.Error("expected Stopped=false")
	}
}

func TestStatusError(t *testing.T) {
	client := mockServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	_, err := client.Status(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
