package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testUID = "E00403500EEA4BF2"

func newMockServer() *httptest.Server {
	h := newHub(testUID)

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

// sseEvent represents one parsed SSE event (event: line + data: line).
type sseEvent struct {
	Name string
	Data string
}

// sseReader continuously scans an SSE stream in a goroutine, accumulating
// events into a thread-safe slice.
type sseReader struct {
	mu     sync.Mutex
	events []sseEvent
}

func newSSEReader(t *testing.T, body io.Reader) *sseReader {
	t.Helper()

	r := &sseReader{}
	go func() {
		sc := bufio.NewScanner(body)
		var curName, curData string

		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				curName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				curData = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "":
				if curName != "" {
					r.mu.Lock()
					r.events = append(r.events, sseEvent{Name: curName, Data: curData})
					r.mu.Unlock()
					curName = ""
					curData = ""
				}
			}
		}
	}()
	return r
}

func (r *sseReader) take() []sseEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := r.events
	r.events = nil
	return out
}

// triggerAndRead connects to the SSE stream and fires the action once, waiting
// for at least want non-keepalive events.
func triggerAndRead(t *testing.T, srv *httptest.Server, action string, want int) []sseEvent {
	t.Helper()

	resp, err := http.Get(srv.URL + ssePath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The SSE handler subscribes before flushing headers, so by the time the
	// GET returns the subscription is active — a single trigger delivers.
	reader := newSSEReader(t, resp.Body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+eventPath+"?action="+action, nil)
	if r, err := http.DefaultClient.Do(req); err == nil {
		r.Body.Close()
	}

	var events []sseEvent
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range reader.take() {
			if e.Name != "keep-alive" {
				events = append(events, e)
			}
		}
		if len(events) >= want {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatalf("got %d events for action %q, want at least %d: %v", len(events), action, want, events)
	return nil
}

func TestTriggerFigurinePlaced(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	events := triggerAndRead(t, srv, "figurine-placed", 5)

	want := []sseEvent{
		{Name: "TagValid", Data: `{ "type":"TagValid", "data":"E00403500EEA4BF2" }`},
		{Name: "playback", Data: `{ "type":"playback", "data":"starting" }`},
		{Name: "playback", Data: `{ "type":"playback", "data":"started" }`},
		{Name: "ContentAudioId", Data: `{ "type":"ContentAudioId", "data":"436906887" }`},
		{Name: "ContentTitle", Data: `{ "type":"ContentTitle", "data":"Unknown" }`},
	}

	if len(events) < len(want) {
		t.Fatalf("got %d events, want at least %d: %v", len(events), len(want), events)
	}

	for i, w := range want {
		if events[i].Name != w.Name || events[i].Data != w.Data {
			t.Errorf("event[%d]: got name=%q data=%q, want name=%q data=%q",
				i, events[i].Name, events[i].Data, w.Name, w.Data)
		}
	}
}

func TestTriggerFigurineLifted(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	events := triggerAndRead(t, srv, "figurine-lifted", 1)

	if len(events) == 0 {
		t.Fatal("no events received")
	}

	// Must be exactly one event: playback stopped. No TagInvalid.
	if events[0].Name != "playback" || events[0].Data != `{ "type":"playback", "data":"stopped" }` {
		t.Errorf("event[0]: got name=%q data=%q, want name=%q data=%q",
			events[0].Name, events[0].Data, "playback", `{ "type":"playback", "data":"stopped" }`)
	}

	for i, e := range events {
		if e.Name == "TagInvalid" {
			t.Errorf("event[%d] is TagInvalid — should not exist", i)
		}
	}

	if len(events) != 1 {
		t.Errorf("expected exactly 1 event, got %d: %v", len(events), events)
	}
}

func TestTriggerRightEarSlap(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	events := triggerAndRead(t, srv, "right-ear-slap", 3)

	want := []sseEvent{
		{Name: "VolumeLevel", Data: `{ "type":"VolumeLevel", "data":"12" }`},
		{Name: "VolumedB", Data: `{ "type":"VolumedB", "data":"-3" }`},
		{Name: "pressed", Data: `{ "type":"pressed", "data":"ear-big" }`},
	}

	if len(events) < len(want) {
		t.Fatalf("got %d events, want at least %d: %v", len(events), len(want), events)
	}

	for i, w := range want {
		if events[i].Name != w.Name || events[i].Data != w.Data {
			t.Errorf("event[%d]: got name=%q data=%q, want name=%q data=%q",
				i, events[i].Name, events[i].Data, w.Name, w.Data)
		}
	}
}

func TestTriggerLeftEarSlap(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	events := triggerAndRead(t, srv, "left-ear-slap", 3)

	want := []sseEvent{
		{Name: "VolumeLevel", Data: `{ "type":"VolumeLevel", "data":"10" }`},
		{Name: "VolumedB", Data: `{ "type":"VolumedB", "data":"-9" }`},
		{Name: "pressed", Data: `{ "type":"pressed", "data":"ear-small" }`},
	}

	if len(events) < len(want) {
		t.Fatalf("got %d events, want at least %d: %v", len(events), len(want), events)
	}

	for i, w := range want {
		if events[i].Name != w.Name || events[i].Data != w.Data {
			t.Errorf("event[%d]: got name=%q data=%q, want name=%q data=%q",
				i, events[i].Name, events[i].Data, w.Name, w.Data)
		}
	}
}

func TestTriggerEarSmallDouble(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	events := triggerAndRead(t, srv, "ear-small-double", 3)

	want := []sseEvent{
		{Name: "VolumeLevel", Data: `{ "type":"VolumeLevel", "data":"9" }`},
		{Name: "VolumedB", Data: `{ "type":"VolumedB", "data":"-12" }`},
		{Name: "pressed", Data: `{ "type":"pressed", "data":"ear-small-double" }`},
	}

	if len(events) < len(want) {
		t.Fatalf("got %d events, want at least %d: %v", len(events), len(want), events)
	}

	for i, w := range want {
		if events[i].Name != w.Name || events[i].Data != w.Data {
			t.Errorf("event[%d]: got name=%q data=%q, want name=%q data=%q",
				i, events[i].Name, events[i].Data, w.Name, w.Data)
		}
	}
}

func TestTriggerKnock(t *testing.T) {
	srv := newMockServer()
	defer srv.Close()

	for _, tc := range []struct {
		action string
		want   sseEvent
	}{
		{"knock-forward", sseEvent{Name: "knock", Data: `{ "type":"knock", "data":"forward" }`}},
		{"knock-backward", sseEvent{Name: "knock", Data: `{ "type":"knock", "data":"backward" }`}},
	} {
		events := triggerAndRead(t, srv, tc.action, 1)
		if len(events) == 0 {
			t.Errorf("%s: no events", tc.action)
			continue
		}
		if events[0].Name != tc.want.Name || events[0].Data != tc.want.Data {
			t.Errorf("%s: got name=%q data=%q, want name=%q data=%q",
				tc.action, events[0].Name, events[0].Data, tc.want.Name, tc.want.Data)
		}
	}
}
