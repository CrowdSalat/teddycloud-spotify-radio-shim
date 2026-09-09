package sselistener

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeSender records translated commands and never fails.
type fakeSender struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSender) Play(uri string) error { f.record("play:" + uri); return nil }
func (f *fakeSender) Pause() error          { f.record("pause"); return nil }
func (f *fakeSender) SkipNext() error       { f.record("skip_next"); return nil }
func (f *fakeSender) SkipPrev() error       { f.record("skip_prev"); return nil }

func (f *fakeSender) record(call string) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	f.mu.Unlock()
}

func (f *fakeSender) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// sseServer serves a fixed set of lines per connection, then closes.
func sseServer(body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, body)
	}))
}

// waitReceipts blocks until at least n commands were recorded and returns them.
func waitReceipts(t *testing.T, f *fakeSender, n int) []string {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := f.snapshot(); len(s) >= n {
			return s[:n]
		}
		time.Sleep(5 * time.Millisecond)
	}

	s := f.snapshot()
	t.Fatalf("timed out waiting for %d receipts, got %v", n, s)
	return nil
}

func newTestListener(serverURL string, f *fakeSender) *Listener {
	return &Listener{
		URL:         serverURL,
		Commands:    f,
		Backoff:     5 * time.Millisecond,
		MaxBackoff:  10 * time.Millisecond,
		IDLETimeout: time.Second,
	}
}

// TestListener_EventMapping feeds one event per supported name and checks the
// translated commands together with the extracted URI.
func TestListener_EventMapping(t *testing.T) {
	const events = "" +
		"event: figurine-placed\ndata: {\"type\":\"figurine-placed\",\"data\":\"spotify:album:X\"}\n\n" +
		"event: figurine-lifted\ndata: {\"type\":\"figurine-lifted\",\"data\":\"\"}\n\n" +
		"event: right-ear-slap\ndata: {\"type\":\"right-ear-slap\",\"data\":\"\"}\n\n" +
		"event: left-ear-slap\ndata: {\"type\":\"left-ear-slap\",\"data\":\"\"}\n\n" +
		"event: TagValid\ndata: {\"type\":\"TagValid\",\"data\":\"0123456789ABCDEF\"}\n\n" +
		"event: TagInvalid\ndata: {\"type\":\"TagInvalid\",\"data\":\"-0123456789ABCDEF\"}\n\n" +
		"event: pressed\ndata: {\"type\":\"pressed\",\"data\":\"ear-big\"}\n\n" +
		"event: pressed\ndata: {\"type\":\"pressed\",\"data\":\"ear-small\"}\n\n"

	srv := sseServer(events)
	defer srv.Close()

	f := &fakeSender{}
	l := newTestListener(srv.URL, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

	want := []string{
		"play:spotify:album:X",
		"pause",
		"skip_next",
		"skip_prev",
		"play:", // TagValid payload is a UID, no URI; Play gets ""
		"pause", // TagInvalid maps to pause
		"skip_next",
		"skip_prev",
	}

	got := waitReceipts(t, f, len(want))
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("receipt[%d]: got %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_IgnoresKeepAliveCommentsAndUnknown checks that transport noise
// and unmapped events produce no commands.
func TestListener_IgnoresKeepAliveCommentsAndUnknown(t *testing.T) {
	const events = "" +
		"event: keep-alive\ndata: {\"type\":\"keep-alive\",\"data\":\"\"}\n\n" +
		": a comment line\n\n" +
		"event: ContentTitle\ndata: {\"type\":\"ContentTitle\",\"data\":\"Benjamin\"}\n\n" +
		"event: ContentAudioId\ndata: {\"type\":\"ContentAudioId\",\"data\":\"42\"}\n\n" +
		"data: {\"type\":\"no-event-name\",\"data\":\"\"}\n\n" +
		"garbage line without a field\n\n" +
		"event: figurine-placed\ndata: {\"type\":\"figurine-placed\",\"data\":\"spotify:album:Y\"}\n\n"

	srv := sseServer(events)
	defer srv.Close()

	f := &fakeSender{}
	l := newTestListener(srv.URL, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

	got := waitReceipts(t, f, 1)
	if got[0] != "play:spotify:album:Y" {
		t.Fatalf("receipt: got %q, want play for figurine-placed only", got[0])
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_CtxCancel checks that Run exits promptly when ctx is cancelled
// while the stream is idle.
func TestListener_CtxCancel(t *testing.T) {
	srv := sseServer("event: keep-alive\ndata: {}\n\n")
	defer srv.Close()

	l := newTestListener(srv.URL, &fakeSender{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop after ctx cancel")
	}
}

// TestListener_Reconnects checks that a dropped connection is resubscribed and
// that commands keep flowing after the reconnect.
func TestListener_Reconnects(t *testing.T) {
	var mu sync.Mutex
	connections := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		mu.Lock()
		connections++
		first := connections == 1
		mu.Unlock()

		if first {
			// First connection: one event, then the handler returns, which
			// closes the connection and forces a reconnect.
			_, _ = fmt.Fprint(w, "event: figurine-placed\ndata: {\"type\":\"figurine-placed\",\"data\":\"spotify:album:A\"}\n\n")
			return
		}

		// Second connection: deliver the next event, then keep it open until
		// the request context is cancelled.
		_, _ = fmt.Fprint(w, "event: figurine-placed\ndata: {\"type\":\"figurine-placed\",\"data\":\"spotify:album:B\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	f := &fakeSender{}
	l := newTestListener(srv.URL, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

	want := []string{"play:spotify:album:A", "play:spotify:album:B"}
	got := waitReceipts(t, f, len(want))
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("receipt[%d]: got %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_PlayBareURIData accepts a payload consisting of a bare
// spotify: URI (not wrapped in the {"data":...} envelope).
func TestListener_PlayBareURIData(t *testing.T) {
	srv := sseServer("event: figurine-placed\ndata: spotify:album:RAW\n\n")
	defer srv.Close()

	f := &fakeSender{}
	l := newTestListener(srv.URL, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

	got := waitReceipts(t, f, 1)
	if got[0] != "play:spotify:album:RAW" {
		t.Fatalf("receipt: got %q, want bare-URI play", got[0])
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_UnknownPressedIsIgnored checks that a pressed event with an
// unmapped payload is dropped rather than mapped arbitrarily.
func TestListener_UnknownPressedIsIgnored(t *testing.T) {
	srv := sseServer("event: pressed\ndata: {\"type\":\"pressed\",\"data\":\"ear-something-else\"}\n\n")
	defer srv.Close()

	f := &fakeSender{}
	l := newTestListener(srv.URL, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

	// No other events arrive; if the loop is alive, no commands must be sent.
	select {
	case <-ctx.Done():
	case <-time.After(150 * time.Millisecond):
	}
	cancel()
	waitStopped(t, done)

	if calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no commands, got %v", calls)
	}
}

// waitStopped blocks until Run returns.
func waitStopped(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
}

// TestSseURL ensures the /api/sse suffix is appended, tolerating a trailing
// slash on the base URL.
func TestSseURL(t *testing.T) {
	l := newTestListener("http://example.com/", &fakeSender{})
	if got := l.sseURL(); got != "http://example.com/api/sse" {
		t.Fatalf("sseURL: got %q", got)
	}
}

// TestWrappedValue covers the envelope formats used by real Teddycloud and the
// mock.
func TestWrappedValue(t *testing.T) {
	table := []struct{ in, want string }{
		{`{"type":"pressed","data":"ear-big"}`, "ear-big"},
		{`{"type":"figurine-placed","data":"spotify:album:X"}`, "spotify:album:X"},
		{`{"type":"TagValid","data":"0123456789ABCDEF"}`, "0123456789ABCDEF"},
		{"spotify:album:RAW", "spotify:album:RAW"},
		{"", ""},
	}
	for _, tc := range table {
		if got := wrappedValue(tc.in); got != tc.want {
			t.Errorf("wrappedValue(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestExtractURI checks the URI extraction rules.
func TestExtractURI(t *testing.T) {
	table := []struct{ in, want string }{
		{`{"type":"figurine-placed","data":"spotify:album:X"}`, "spotify:album:X"},
		{"spotify:album:RAW", "spotify:album:RAW"},
		{`{"type":"TagValid","data":"0123456789ABCDEF"}`, ""},
	}
	for _, tc := range table {
		if got := extractURI(tc.in); got != tc.want {
			t.Errorf("extractURI(%q): got %q, want %q", tc.in, got, tc.want)
		}
	}
}
