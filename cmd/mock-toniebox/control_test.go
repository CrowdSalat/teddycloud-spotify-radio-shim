package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/toniebox"
)

const testMAC = "00aabbccdd00"

// testRUID is the figurine the stub box carries. 0x7205759405470563 renders as
// E001000100000001, which is what teddyCloud's log shows for it.
const testRUID = "E001000100000001"

// newStubMock returns a mock wired to a throwaway listener, so the real control
// handlers and the real client run while nothing is listening on teddyCloud's
// side. The RTNL connection is a genuine TCP dial: frames are written for real,
// which is what the send path is worth testing.
func newStubMock(t *testing.T) *mock {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client := toniebox.NewClient(toniebox.ClientConfig{BaseURL: server.URL})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("closing the client: %v", err)
		}
	})

	uid, err := toniebox.ParseRUID(testRUID)
	if err != nil {
		t.Fatalf("parsing the test rUID: %v", err)
	}

	box := toniebox.NewBox(toniebox.DefaultConfig(testMAC, uid))
	return &mock{
		box:        box,
		client:     client,
		downloader: toniebox.NewDownloader(box, client, 0),
		rec:        newRecorder(box.Config(), testMAC),
	}
}

// do issues a request against the control surface and returns the recorder.
func post(t *testing.T, m *mock, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	m.handleControl(rec, httptest.NewRequest(http.MethodPost, target, nil))
	return rec
}

func get(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// TestControlRejectsGET keeps the action surface POST-only, so a link or a
// prefetch cannot start a cycle run.
func TestControlRejectsGET(t *testing.T) {
	m := newStubMock(t)

	rec := httptest.NewRecorder()
	m.handleControl(rec, httptest.NewRequest(http.MethodGet, "/control/cycle?count=200", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /control/cycle returned %d, want 405", rec.Code)
	}
}

// TestControlRejectsUnknownAction checks that a typo reports itself. Silently
// doing nothing would leave an operator believing an event had been sent.
func TestControlRejectsUnknownAction(t *testing.T) {
	m := newStubMock(t)

	for _, path := range []string{
		"/control/nonsense",
		"/control/knock/sideways",
		"/control/playback/paused",
		"/control/ear/middle",
		"/control/tilt/up",
		"/control/charger/charging",
	} {
		t.Run(path, func(t *testing.T) {
			rec := post(t, m, path)
			if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
				t.Errorf("%s returned %d, want 400 or 404", path, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "error") {
				t.Errorf("%s body does not carry an error: %s", path, rec.Body.String())
			}
		})
	}
}

// TestControlRejectsBadUID covers the rUID validation at the boundary. A malformed
// id has to be refused rather than reaching teddyCloud, where it would produce a
// 404 that looks like a missing figurine rather than a bad request.
func TestControlRejectsBadUID(t *testing.T) {
	m := newStubMock(t)

	for _, uid := range []string{"ZZZZ", "E00403500EEA4BF", "E00403500EEA4BF22", "not%20a%20uid"} {
		t.Run("uid="+uid, func(t *testing.T) {
			rec := post(t, m, "/control/invalid?uid="+uid)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("uid %q returned %d, want 400", uid, rec.Code)
			}
		})
	}

	// An absent uid means "the figurine this box carries", which is valid and
	// must not be confused with a malformed one.
	if rec := post(t, m, "/control/invalid"); rec.Code == http.StatusBadRequest {
		t.Error("an absent uid was refused, want the configured default")
	}
}

// TestControlAcceptsGoodUID is the positive half, so the validation is not simply
// refusing everything.
func TestControlAcceptsGoodUID(t *testing.T) {
	m := newStubMock(t)

	rec := post(t, m, "/control/invalid?uid=E00403500EEA4BF2")
	if rec.Code == http.StatusBadRequest {
		t.Errorf("a valid rUID was refused: %s", rec.Body.String())
	}
}

// TestControlAudioIDRequiresAValue checks the parameter guard on the audio-id
// report, which is otherwise a silent no-op.
func TestControlAudioIDRequiresAValue(t *testing.T) {
	m := newStubMock(t)

	for _, q := range []string{"", "?id=", "?id=abc", "?id=-1", "?id=99999999999"} {
		t.Run("audio-id"+q, func(t *testing.T) {
			rec := post(t, m, "/control/audio-id"+q)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("/control/audio-id%s returned %d, want 400", q, rec.Code)
			}
		})
	}

	if rec := post(t, m, "/control/audio-id?id=42"); rec.Code == http.StatusBadRequest {
		t.Error("a valid audio id was refused")
	}
}

// TestControlReportAudioIDNeedsADownload covers the endpoint that reuses the audio
// id from the last TAF. Before anything has been downloaded there is no id, and
// the operator should be told that rather than getting a silent zero.
func TestControlReportAudioIDNeedsADownload(t *testing.T) {
	m := newStubMock(t)

	rec := post(t, m, "/control/report-audio-id")
	if rec.Code != http.StatusNotFound {
		t.Errorf("returned %d, want 404 with nothing downloaded", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "TAF") {
		t.Errorf("body does not explain the missing download: %s", rec.Body.String())
	}

	// Once a TAF has been read, the same endpoint has an id to report.
	m.box.NoteDownloadedTAF(431165001)
	rec = post(t, m, "/control/report-audio-id")
	if rec.Code != http.StatusOK {
		t.Fatalf("returned %d after a download, want 200: %s", rec.Code, rec.Body.String())
	}

	var body struct {
		AudioID uint32 `json:"audio_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the reply: %v", err)
	}
	if body.AudioID != 431165001 {
		t.Errorf("audio_id = %d, want 431165001", body.AudioID)
	}
}

// TestControlStallAndResume covers the gate endpoints, which are the difference
// between "the box paused" and "the box left" and so must not be one-way.
func TestControlStallAndResume(t *testing.T) {
	m := newStubMock(t)

	if rec := post(t, m, "/control/stall"); rec.Code != http.StatusOK {
		t.Fatalf("stall returned %d, want 200", rec.Code)
	}
	if !m.downloader.Gate().Suspended() {
		t.Error("the gate is open after /control/stall")
	}

	if rec := post(t, m, "/control/resume"); rec.Code != http.StatusOK {
		t.Fatalf("resume returned %d, want 200", rec.Code)
	}
	if m.downloader.Gate().Suspended() {
		t.Error("the gate is suspended after /control/resume")
	}

	// A timed stall releases itself, which is what /control/stall?ms= does.
	post(t, m, "/control/stall?ms=20")
	if !m.downloader.Gate().Suspended() {
		t.Fatal("a timed stall did not suspend the gate")
	}
	deadline := time.Now().Add(time.Second)
	for m.downloader.Gate().Suspended() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if m.downloader.Gate().Suspended() {
		t.Error("the timed stall never released the gate")
	}
}

// TestControlReportEndpoint serves the measurement table as a read, and must work
// on an empty run rather than erroring.
func TestControlReportEndpoint(t *testing.T) {
	m := newStubMock(t)

	rec := get(t, m.handleReport, "/control/report")
	if rec.Code != http.StatusOK {
		t.Fatalf("returned %d, want 200", rec.Code)
	}

	var body struct {
		OverlayID    string        `json:"overlay_id"`
		Assumptions  [][2]string   `json:"assumptions"`
		Measurements []measurement `json:"measurements"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the report: %v", err)
	}
	if body.OverlayID != "00aabbccdd00" {
		t.Errorf("overlay_id = %q, want the box MAC", body.OverlayID)
	}
	if len(body.Assumptions) == 0 {
		t.Error("the report carries no assumptions")
	}
	if len(body.Measurements) == 0 {
		t.Error("the report carries no measurements")
	}
}

// TestControlIndexServesThePage checks the operator surface exists and names the
// actions, since the design's premise is that the mock is drivable by a person.
func TestControlIndexServesThePage(t *testing.T) {
	m := newStubMock(t)

	rec := httptest.NewRecorder()
	m.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / returned %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{"/control/place", "/control/lift", "/control/cycle", "/api/sse"} {
		if !strings.Contains(body, want) {
			t.Errorf("the control page does not offer %s", want)
		}
	}

	// Anything that is not the root is a 404 rather than the page.
	rec = httptest.NewRecorder()
	m.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/elsewhere", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /elsewhere returned %d, want 404", rec.Code)
	}
}

// TestNextBackoff covers the reconnect ramp used by both the RTNL writer and the
// SSE observer. An off-by-one here turns a transient outage into a busy loop.
func TestNextBackoff(t *testing.T) {
	cases := []struct {
		current, max, want time.Duration
	}{
		{time.Second, 30 * time.Second, 2 * time.Second},
		{16 * time.Second, 30 * time.Second, 30 * time.Second},
		{30 * time.Second, 30 * time.Second, 30 * time.Second},
		{1 * time.Millisecond, time.Millisecond, time.Millisecond},
	}

	for _, tc := range cases {
		if got := nextBackoff(tc.current, tc.max); got != tc.want {
			t.Errorf("nextBackoff(%v, %v) = %v, want %v", tc.current, tc.max, got, tc.want)
		}
	}
}

// TestSleepCtxReturnsOnCancel covers the cancellation-aware sleep the writer loop
// depends on: a signal has to shut the process down promptly rather than after the
// current backoff.
func TestSleepCtxReturnsOnCancel(t *testing.T) {
	t.Run("cancelled context returns at once", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		start := time.Now()
		if sleepCtx(ctx, time.Minute) {
			t.Error("sleepCtx reported completion for a cancelled context")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("sleepCtx took %v to notice cancellation", elapsed)
		}
	})

	t.Run("cancellation mid-sleep is noticed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		start := time.Now()
		if sleepCtx(ctx, time.Minute) {
			t.Error("sleepCtx reported completion for a cancelled context")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("sleepCtx took %v to notice a cancel 20 ms in", elapsed)
		}
	})

	t.Run("an uncancelled sleep completes", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan bool, 1)
		go func() { done <- sleepCtx(ctx, time.Millisecond) }()
		select {
		case ok := <-done:
			if !ok {
				t.Error("sleepCtx on an uncancelled context returned false")
			}
		case <-time.After(time.Second):
			t.Fatal("sleepCtx did not return for a short sleep")
		}
	})
}
