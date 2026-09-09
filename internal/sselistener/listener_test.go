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

// TestListener_TagValidPlay checks that TagValid triggers Play with empty URI.
func TestListener_TagValidPlay(t *testing.T) {
	const events = "" +
		"event: TagValid\ndata: { \"type\":\"TagValid\", \"data\":\"E00403500EEA4BF2\" }\n\n"

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
	if got[0] != "play:" {
		t.Fatalf("receipt: got %q, want play: (empty URI from TagValid)", got[0])
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_PlaybackStoppedPause checks that playback stopped triggers Pause.
func TestListener_PlaybackStoppedPause(t *testing.T) {
	const events = "" +
		"event: playback\ndata: { \"type\":\"playback\", \"data\":\"stopped\" }\n\n"

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
	if got[0] != "pause" {
		t.Fatalf("receipt: got %q, want pause", got[0])
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_PlaybackStartingStartedIgnored checks that playback
// starting/started produce no commands.
func TestListener_PlaybackStartingStartedIgnored(t *testing.T) {
	const events = "" +
		"event: playback\ndata: { \"type\":\"playback\", \"data\":\"starting\" }\n\n" +
		"event: playback\ndata: { \"type\":\"playback\", \"data\":\"started\" }\n\n"

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

	// Wait briefly to confirm no commands arrive.
	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	waitStopped(t, done)

	if calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no commands, got %v", calls)
	}
}

// TestListener_PressedEarBigSkipNext checks that pressed ear-big triggers
// SkipNext.
func TestListener_PressedEarBigSkipNext(t *testing.T) {
	const events = "" +
		"event: pressed\ndata: { \"type\":\"pressed\", \"data\":\"ear-big\" }\n\n"

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
	if got[0] != "skip_next" {
		t.Fatalf("receipt: got %q, want skip_next", got[0])
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_PressedEarSmallSkipPrev checks that pressed ear-small triggers
// SkipPrev.
func TestListener_PressedEarSmallSkipPrev(t *testing.T) {
	const events = "" +
		"event: pressed\ndata: { \"type\":\"pressed\", \"data\":\"ear-small\" }\n\n"

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
	if got[0] != "skip_prev" {
		t.Fatalf("receipt: got %q, want skip_prev", got[0])
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_PressedEarSmallDoubleIgnored checks that pressed ear-small-double
// produces no command.
func TestListener_PressedEarSmallDoubleIgnored(t *testing.T) {
	const events = "" +
		"event: pressed\ndata: { \"type\":\"pressed\", \"data\":\"ear-small-double\" }\n\n"

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

	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	waitStopped(t, done)

	if calls := f.snapshot(); len(calls) != 0 {
		t.Fatalf("expected no commands, got %v", calls)
	}
}

// TestListener_IgnoresKnockVolumeAndKeepAlive checks that transport noise and
// unmapped events produce no commands.
func TestListener_IgnoresKnockVolumeAndKeepAlive(t *testing.T) {
	const events = "" +
		"event: keep-alive\ndata: { \"type\":\"keep-alive\", \"data\":\"\" }\n\n" +
		"event: VolumeLevel\ndata: { \"type\":\"VolumeLevel\", \"data\":\"12\" }\n\n" +
		"event: VolumedB\ndata: { \"type\":\"VolumedB\", \"data\":\"-3\" }\n\n" +
		"event: knock\ndata: { \"type\":\"knock\", \"data\":\"forward\" }\n\n" +
		"event: ContentAudioId\ndata: { \"type\":\"ContentAudioId\", \"data\":\"436906887\" }\n\n" +
		"event: ContentTitle\ndata: { \"type\":\"ContentTitle\", \"data\":\"Unknown\" }\n\n" +
		": a comment line\n\n" +
		"garbage line without a field\n\n" +
		"event: TagValid\ndata: { \"type\":\"TagValid\", \"data\":\"E00403500EEA4BF2\" }\n\n"

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

	// Only the TagValid at the end should produce a command.
	got := waitReceipts(t, f, 1)
	if got[0] != "play:" {
		t.Fatalf("receipt: got %q, want play:", got[0])
	}

	cancel()
	waitStopped(t, done)
}

// TestListener_UnknownPressedIsIgnored checks that a pressed event with an
// unmapped payload is dropped rather than mapped arbitrarily.
func TestListener_UnknownPressedIsIgnored(t *testing.T) {
	srv := sseServer("event: pressed\ndata: { \"type\":\"pressed\", \"data\":\"ear-something-else\" }\n\n")
	defer srv.Close()

	f := &fakeSender{}
	l := newTestListener(srv.URL, f)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Run(ctx)
	}()

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
			_, _ = fmt.Fprint(w, "event: TagValid\ndata: {\"type\":\"TagValid\",\"data\":\"UID_A\"}\n\n")
			return
		}

		// Second connection: deliver the next event, then keep it open until
		// the request context is cancelled.
		_, _ = fmt.Fprint(w, "event: TagValid\ndata: {\"type\":\"TagValid\",\"data\":\"UID_B\"}\n\n")
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

	want := []string{"play:", "play:"}
	got := waitReceipts(t, f, len(want))
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("receipt[%d]: got %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}

	cancel()
	waitStopped(t, done)
}

// TestSseURL ensures the /api/sse suffix is appended, tolerating a trailing
// slash on the base URL.
func TestSseURL(t *testing.T) {
	l := newTestListener("http://example.com/", &fakeSender{})
	if got := l.sseURL(); got != "http://example.com/api/sse" {
		t.Fatalf("sseURL: got %q", got)
	}
}

// TestWrappedValue covers the envelope format used by real Teddycloud.
func TestWrappedValue(t *testing.T) {
	table := []struct{ in, want string }{
		{`{"type":"pressed","data":"ear-big"}`, "ear-big"},
		{`{"type":"TagValid","data":"E00403500EEA4BF2"}`, "E00403500EEA4BF2"},
		{`{"type":"playback","data":"stopped"}`, "stopped"},
		{`{"type":"playback","data":"starting"}`, "starting"},
		{`{"type":"knock","data":"forward"}`, "forward"},
		{"spotify:album:RAW", "spotify:album:RAW"},
		{"", ""},
	}
	for _, tc := range table {
		if got := wrappedValue(tc.in); got != tc.want {
			t.Errorf("wrappedValue(%q): got %q, want %q", tc.in, got, tc.want)
		}
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
