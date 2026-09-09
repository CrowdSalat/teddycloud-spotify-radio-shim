package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testURI = "spotify:album:TEST"

func newMockServer() *httptest.Server {
	h := newHub(testURI)

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex(h))
	mux.HandleFunc(eventPath, handleEvent(h))
	mux.HandleFunc(ssePath, handleSSE(h))

	return httptest.NewServer(mux)
}

func TestIndexHTML(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}

	buf := new(strings.Builder)
	_, _ = io.Copy(buf, resp.Body)

	for _, want := range []string{"Place figurine", "Lift figurine", "Right ear", "Left ear"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("index HTML missing button %q", want)
		}
	}
}

func TestSSEStreamHeaders(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	resp, err := http.Get(srv.URL + ssePath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type: got %q, want text/event-stream", ct)
	}
}

// TestTriggerDeliversFigurinePlaced checks that triggering the event sends an
// SSE event whose payload contains the configured URI.
func TestTriggerDeliversFigurinePlaced(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	resp, err := http.Get(srv.URL + ssePath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Trigger repeatedly: the subscription is registered asynchronously, so a
	// single trigger might race the subscriber. Keep POSTing until the event
	// shows up on the stream.
	quit := make(chan struct{})
	go func() {
		for {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+eventPath+"?action=figurine-placed", nil)
			if r, err := http.DefaultClient.Do(req); err == nil {
				r.Body.Close()
			}

			select {
			case <-quit:
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	defer close(quit)

	sc := bufio.NewScanner(resp.Body)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !sc.Scan() {
			t.Fatal("sse stream closed early")
		}

		line := sc.Text()
		if strings.Contains(line, "figurine-placed") && strings.Contains(line, testURI) {
			return
		}
	}

	t.Fatal("no figurine-placed SSE event containing the URI was received")
}
