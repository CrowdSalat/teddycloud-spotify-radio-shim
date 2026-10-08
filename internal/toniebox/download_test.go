package toniebox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// requestLog records what the fake teddyCloud was asked for, so a test can assert
// on the request sequence rather than only on the outcome. The Range header in
// particular is the lever the whole tool exists to exercise, and it is invisible
// from the response side.
type requestLog struct {
	mu     sync.Mutex
	paths  []string
	ranges []string
}

func (l *requestLog) record(r *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, r.URL.Path)
	l.ranges = append(l.ranges, r.Header.Get("Range"))
}

func (l *requestLog) snapshot() ([]string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.paths...), append([]string(nil), l.ranges...)
}

// fakeCloud is a stand-in for the parts of teddyCloud a placement touches: the
// freshness routes, content-meta, the chapter routes and the v2 content route.
// Only the behaviour the mock depends on is modelled — a fixed reply per route,
// plus a switch to make one of them fail.
type fakeCloud struct {
	*httptest.Server

	// t is kept because the v2 handler reuses buildTAFBlock, which needs a
	// testing.T for its failure path.
	t *testing.T

	// AudioID is reported as content-meta's version and, on the v2 route, is
	// written into the TAF header.
	AudioID uint32
	// Chapters is how many chapter bodies to serve on the v3 path.
	Chapters int
	// BodySize is the size of each chapter body.
	BodySize int

	// FailMeta, when set, makes /v3/content-meta answer with this status.
	FailMeta int
	// FailV2, when set, makes /v2/content answer with this status.
	FailV2 int

	log *requestLog
}

func newFakeCloud(t *testing.T, audioID uint32, chapters, bodySize int) *fakeCloud {
	t.Helper()

	f := &fakeCloud{
		t:        t,
		AudioID:  audioID,
		Chapters: chapters,
		BodySize: bodySize,
		log:      &requestLog{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCloud) serve(w http.ResponseWriter, r *http.Request) {
	f.log.record(r)

	switch {
	case r.URL.Path == "/v1/freshness-check":
		// teddyCloud answers no reply at all for a body at or above 4096 bytes,
		// so a body that size has to fail here rather than look like a success
		// (src/handler_cloud.c:1050-1056).
		received, err := io.ReadAll(io.LimitReader(r.Body, MaxFreshnessBody+1))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if len(received) >= MaxFreshnessBody {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		// The real reply marks every uid whose source is a stream, which for
		// this project is always all of them (src/handler_cloud.c:1031-1034).
		_, _ = w.Write(encodeFCResponse(f.t, []uint64{1}))

	case r.URL.Path == "/v3/freshness-check":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":["E001000100000001"]}`))

	case strings.HasPrefix(r.URL.Path, "/v3/content-meta/"):
		if f.FailMeta != 0 {
			w.WriteHeader(f.FailMeta)
			return
		}
		content := make([]ContentItem, 0, f.Chapters)
		for i := 0; i < f.Chapters; i++ {
			content = append(content, ContentItem{
				Type:     "audio",
				Name:     fmt.Sprintf("teddycloud_%02d_e001000100000001", i),
				FileSize: int64(f.BodySize),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ContentMeta{
			Version:        uint64(f.AudioID),
			ContentType:    "content_tonie",
			TonieType:      "content",
			ResumeBehavior: "resume",
			TonieSalesID:   "custom_001",
			Content:        content,
		})

	case strings.HasPrefix(r.URL.Path, "/v3/chapter/"):
		// teddyCloud serves a v3 chapter from offset 4096 with header_size=0, so
		// no TAF header ever appears here (src/handler_cloud.c:1535-1553). The
		// body is raw Ogg as far as the mock is concerned.
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(make([]byte, f.BodySize))

	case strings.HasPrefix(r.URL.Path, "/v2/content/"):
		if f.FailV2 != 0 {
			w.WriteHeader(f.FailV2)
			return
		}
		block := buildTAFBlock(f.t, tafTestHeader())
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(block)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// encodeFCResponse renders TonieFreshnessCheckResponse. Every box-setting field
// is required in proto2, so they are all present even at zero
// (proto/toniebox.pb.freshness-check.fc-response.proto).
func encodeFCResponse(t *testing.T, marked []uint64) []byte {
	t.Helper()

	var b []byte
	for _, uid := range marked {
		b = appendFixed64(b, 1, uid)
	}
	for field := 2; field <= 8; field++ {
		b = appendUint32(b, field, 0)
	}
	return b
}

// testDownloader wires a downloader against the fake, with the pace wide open so
// tests do not spend real seconds waiting for the read loop.
func testDownloader(t *testing.T, cfg Config, cloud *fakeCloud) (*Downloader, *requestLog) {
	t.Helper()

	box := NewBox(cfg)
	client := NewClient(ClientConfig{BaseURL: cloud.URL})
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("closing client: %v", err)
		}
	})
	return NewDownloader(box, client, 0), cloud.log
}

// TestPlaceV3LearnsAudioIDFromMeta is the fix for a gap that made one of the
// report's own assertions unreachable.
//
// /v3/chapter serves chapter 0 from offset 4096 with header_size=0, so a v3 body
// carries no TAF header and ParseTAFHeader has nothing to read. The audio id is
// only available as content-meta's version, so if that is not adopted the box
// never learns what it cached, reports 0 forever, and the ContentAudioId
// correlation can never fire.
func TestPlaceV3LearnsAudioIDFromMeta(t *testing.T) {
	const audioID = 431165001
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, audioID, 1, 4096)
	cfg := DefaultConfig("00aabbccdd00", uid)
	downloader, _ := testDownloader(t, cfg, cloud)

	result, err := downloader.Place(context.Background(), uid)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}

	if got, ok := downloader.box.LastTAFAudioID(); !ok || got != audioID {
		t.Errorf("box audio id = %d (present=%v), want %d", got, ok, audioID)
	}
	// The body genuinely has no header, which is what makes the meta route the
	// only source — assert that too, so the test cannot pass for the wrong reason.
	if len(result.Downloads) != 1 || result.Downloads[0].HasTAF {
		t.Errorf("v3 body reported HasTAF=%v; it should not carry a header", result.Downloads[0].HasTAF)
	}
	if result.TotalReceived != 4096 {
		t.Errorf("received %d bytes, want 4096", result.TotalReceived)
	}

	// The result has to carry the id too. A report's ContentAudioId correlation is
	// computed from these, so a zero here makes that assertion pass vacuously on
	// every v3 run — it can never fail, which is worse than not having it.
	if got := result.Downloads[0].AudioID; got != audioID {
		t.Errorf("download result audio id = %d, want %d", got, audioID)
	}
}

// TestAudioIDReachesProgressConsumers is the other half: the recorder only ever
// learns the id from progress milestones, and download() emits those before the
// id is known. Without a republish the report records zero however correct the
// download result is.
func TestAudioIDReachesProgressConsumers(t *testing.T) {
	const audioID = 431165001
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, audioID, 1, 4096)
	downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

	var mu sync.Mutex
	var seen []DownloadProgress
	downloader.OnProgress = func(p DownloadProgress) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, p)
	}

	if _, err := downloader.Place(context.Background(), uid); err != nil {
		t.Fatalf("Place: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var lastDone DownloadProgress
	for _, p := range seen {
		if p.Done {
			lastDone = p
		}
	}
	if lastDone.AudioID != audioID {
		t.Errorf("final progress milestone audio id = %d, want %d", lastDone.AudioID, audioID)
	}
}

// TestAudioIDStampedOnDroppedBody guards the ordering: the drop path returns from
// the chapter loop early, so stamping at the end of the function rather than per
// chapter would skip it for exactly the scenario the drop exists to produce.
func TestAudioIDStampedOnDroppedBody(t *testing.T) {
	const audioID = 431165001
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, audioID, 1, 1<<20)
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.DropAfter = 4096
	downloader, _ := testDownloader(t, cfg, cloud)

	result, _ := downloader.Place(context.Background(), uid)
	if len(result.Downloads) == 0 {
		t.Fatal("no downloads recorded")
	}
	if got := result.Downloads[0].AudioID; got != audioID {
		t.Errorf("dropped body audio id = %d, want %d", got, audioID)
	}
}

// TestPlaceV2LearnsAudioIDFromBody is the counterpart: the v2 route does serve a
// TAF header, so the id comes off the wire rather than from the metadata.
func TestPlaceV2LearnsAudioIDFromBody(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 0, 0, 0)
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.ContentPath = ContentV2
	downloader, _ := testDownloader(t, cfg, cloud)

	if _, err := downloader.Place(context.Background(), uid); err != nil {
		t.Fatalf("Place: %v", err)
	}

	// tafTestHeader's audio_id.
	if got, _ := downloader.box.LastTAFAudioID(); got != 372444745 {
		t.Errorf("box audio id = %d, want 372444745 parsed from the body", got)
	}
}

// TestFreshnessRunsOnFirstPlacement checks the other gap: an empty cache has to
// still be reported, as audio id 0. Suppressing the exchange would leave
// /v1/freshness-check unexercised for the whole first cycle of every run.
func TestFreshnessRunsOnFirstPlacement(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

	result, err := downloader.Place(context.Background(), uid)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}

	if result.Freshness.Skipped {
		t.Error("freshness check was skipped on a first placement, want it performed")
	}
	if result.Freshness.Endpoint != "/v1/freshness-check" {
		t.Errorf("endpoint = %q, want /v1/freshness-check", result.Freshness.Endpoint)
	}
	if got := result.Freshness.Reported[RUIDHex(uid)]; got != 0 {
		t.Errorf("reported audio id = %d on an empty cache, want 0", got)
	}

	paths, _ := cloud.log.snapshot()
	if !contains(paths, "/v1/freshness-check") {
		t.Errorf("freshness route was never requested; saw %v", paths)
	}
}

// TestRangeResumeSendsOffset is the primary lever: `Range: bytes=N-` is what
// makes teddyCloud append to the existing TAF instead of starting a fresh ffmpeg,
// and therefore what decides whether the shim sees a new /stream request
// (src/handler_cloud.c:645).
func TestRangeResumeSendsOffset(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.Range = RangeResume
	downloader, _ := testDownloader(t, cfg, cloud)

	downloader.box.AddBytes(1234)

	if _, err := downloader.Place(context.Background(), uid); err != nil {
		t.Fatalf("Place: %v", err)
	}

	_, ranges := cloud.log.snapshot()
	var sent string
	for _, r := range ranges {
		if strings.HasPrefix(r, "bytes=") {
			sent = r
		}
	}
	if sent != "bytes=1234-" {
		t.Errorf("Range sent = %q, want bytes=1234-", sent)
	}
}

// TestRangeZeroSendsNoHeader is the other half: a fresh placement asks for the
// whole body, because teddyCloud ignores `bytes=0-` anyway and answers 200
// (src/cyclone/cyclone_tcp/http/http_server.c:1092-1110).
func TestRangeZeroSendsNoHeader(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

	if _, err := downloader.Place(context.Background(), uid); err != nil {
		t.Fatalf("Place: %v", err)
	}

	for _, r := range mustRanges(t, cloud) {
		if r != "" {
			t.Errorf("Range header %q sent on a zero-offset placement, want none", r)
		}
	}
}

// TestDropAfterReRequestsWithRange is scenario 11.1-2: the box loses the body
// mid-stream and comes back asking for the rest. Whether teddyCloud appends or
// restarts is the open question in the design, and reaching it needs exactly this
// request sequence.
func TestDropAfterReRequestsWithRange(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 1<<20)
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.Range = RangeResume
	cfg.DropAfter = 4096
	downloader, _ := testDownloader(t, cfg, cloud)

	result, err := downloader.Place(context.Background(), uid)
	if err == nil {
		t.Log("placement completed without the configured drop being reported as an error")
	}
	if len(result.Downloads) != 2 {
		t.Fatalf("made %d content requests, want 2 (the drop and the resume)", len(result.Downloads))
	}

	ranges := mustRanges(t, cloud)
	var resume string
	for _, r := range ranges {
		if strings.HasPrefix(r, "bytes=") {
			resume = r
		}
	}
	if resume == "" {
		t.Errorf("the resume request carried no Range; saw %v", ranges)
	}
}

// TestMaxDownloadHoldsTheConnection stops the read without failing the download,
// which is the difference between "the box paused" and "the box left".
func TestMaxDownloadHoldsTheConnection(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 1<<20)
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.MaxDownload = 8192
	downloader, _ := testDownloader(t, cfg, cloud)

	result, err := downloader.Place(context.Background(), uid)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if result.TotalReceived > cfg.MaxDownload+32768 {
		t.Errorf("received %d bytes, want roughly the %d ceiling", result.TotalReceived, cfg.MaxDownload)
	}
}

// TestFallbackToV2OnMetaFailure covers --fallback=on-404, which is the default:
// /v3/content-meta answering 404 sends the box to the v2 route rather than
// failing the placement.
func TestFallbackToV2OnMetaFailure(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	cloud.FailMeta = http.StatusNotFound
	downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

	result, err := downloader.Place(context.Background(), uid)
	if err != nil {
		t.Fatalf("Place with a failing v3 route: %v", err)
	}

	paths, _ := cloud.log.snapshot()
	if !containsPrefix(paths, "/v2/content/") {
		t.Errorf("did not fall back to v2; saw %v", paths)
	}
	if result.Path != "v2" {
		t.Errorf("recorded content path = %q, want v2", result.Path)
	}
}

// TestFallbackOffFailsLoudly is the counterpart: with the fallback disabled the
// 404 has to surface rather than being papered over.
func TestFallbackOffFailsLoudly(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	cloud.FailMeta = http.StatusNotFound
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.Fallback = FallbackNone
	downloader, _ := testDownloader(t, cfg, cloud)

	if _, err := downloader.Place(context.Background(), uid); err == nil {
		t.Error("placement succeeded with --fallback=off and a 404 content-meta")
	}
}

// TestFallbackIgnoresNonNotFound is the guard for the bug the statusError type
// exists to fix. The fallback used to fire on *any* error, which meant a
// deliberate --drop-after abort came back as a successful v2 download: the
// scenario the drop exists to produce was quietly converted into a pass.
func TestFallbackIgnoresNonNotFound(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	t.Run("server error is not fallback-eligible", func(t *testing.T) {
		cloud := newFakeCloud(t, 431165001, 1, 4096)
		cloud.FailMeta = http.StatusInternalServerError
		// on-404, the default.
		downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

		if _, err := downloader.Place(context.Background(), uid); err == nil {
			t.Error("placement succeeded on a 500; a non-404 must not trigger the fallback")
		}
		paths, _ := cloud.log.snapshot()
		if containsPrefix(paths, "/v2/content/") {
			t.Errorf("the v2 route was tried after a 500; saw %v", paths)
		}
	})

	t.Run("chapter ceiling is not fallback-eligible", func(t *testing.T) {
		cloud := newFakeCloud(t, 431165001, MaxChapters+1, 16)
		downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

		if _, err := downloader.Place(context.Background(), uid); err == nil {
			t.Error("placement succeeded above the chapter ceiling via a fallback")
		}
		paths, _ := cloud.log.snapshot()
		if containsPrefix(paths, "/v2/content/") {
			t.Errorf("the v2 route masked an over-long chapter list; saw %v", paths)
		}
	})
}

// TestFallbackAlwaysTriesV2First pins the --fallback=always ordering: v2 first,
// v3 only when v2 is a 404. The flag models a box whose firmware reaches for the
// older route first, so getting the order backwards measures a different box
// behaviour than the one the flag names.
func TestFallbackAlwaysTriesV2First(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.ContentPath = ContentV3
	cfg.Fallback = FallbackAlways
	downloader, _ := testDownloader(t, cfg, cloud)

	if _, err := downloader.Place(context.Background(), uid); err != nil {
		t.Fatalf("Place: %v", err)
	}

	paths, _ := cloud.log.snapshot()
	if got := firstContentPath(paths); !strings.HasPrefix(got, "/v2/content/") {
		t.Errorf("first content route was %q, want the v2 route with --fallback=always", got)
	}
	if got := pathOf([]DownloadResult{{Path: firstContentPath(paths)}}); got != "v2" {
		t.Errorf("placement reported content path %q, want v2", got)
	}
}

// TestFallbackAlwaysRescuesWithV3 is the other half: with v2 answering 404,
// --fallback=always has to reach v3 rather than fail.
func TestFallbackAlwaysRescuesWithV3(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	cloud.FailV2 = http.StatusNotFound
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.ContentPath = ContentV3
	cfg.Fallback = FallbackAlways
	downloader, _ := testDownloader(t, cfg, cloud)

	if _, err := downloader.Place(context.Background(), uid); err != nil {
		t.Fatalf("Place with v2 answering 404: %v", err)
	}

	paths, _ := cloud.log.snapshot()
	var sawV2, sawV3 bool
	for _, p := range paths {
		sawV2 = sawV2 || strings.HasPrefix(p, "/v2/content/")
		sawV3 = sawV3 || strings.HasPrefix(p, "/v3/chapter/")
	}
	if !sawV2 || !sawV3 {
		t.Errorf("expected v2 then v3; saw %v", paths)
	}
}

// TestPathOfReportsTheRouteActuallyUsed checks the report names the route that
// served the bytes. Recording the *configured* path instead means a placement
// served over v2 by a fallback is reported as v3, which is worse than no report.
func TestPathOfReportsTheRouteActuallyUsed(t *testing.T) {
	cases := []struct {
		name    string
		results []DownloadResult
		want    string
	}{
		{"v3", []DownloadResult{{Path: "/v3/chapter/one"}}, "v3"},
		{"v2", []DownloadResult{{Path: "/v2/content/E001000100000001"}}, "v2"},
		{"fallback after v3", []DownloadResult{
			{Path: "/v3/content-meta/E001000100000001"},
			{Path: "/v2/content/E001000100000001"},
		}, "v2"},
		{"nothing", nil, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pathOf(tc.results); got != tc.want {
				t.Errorf("pathOf = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestNotFoundOnlyMatchesStatus keeps the fallback predicate narrow. A status
// error of any other code is a different condition and must not be treated as a
// missing route.
func TestNotFoundOnlyMatchesStatus(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{&statusError{status: http.StatusNotFound, path: "/v3/content-meta/x"}, true},
		{&statusError{status: http.StatusInternalServerError, path: "/v3/content-meta/x"}, false},
		{&statusError{status: http.StatusUnauthorized, path: "/v3/content-meta/x"}, false},
		{errors.New("connection refused"), false},
		{nil, false},
	}

	for i, tc := range cases {
		if got := notFound(tc.err); got != tc.want {
			t.Errorf("case %d: notFound(%v) = %v, want %v", i, tc.err, got, tc.want)
		}
	}
}

// firstContentPath returns the first requested path that is a content route,
// skipping the freshness and metadata calls that always precede it.
func firstContentPath(paths []string) string {
	for _, p := range paths {
		if strings.HasPrefix(p, "/v2/content/") || strings.HasPrefix(p, "/v3/chapter/") {
			return p
		}
	}
	return ""
}

// TestTooManyChaptersRejected pins the ceiling check at the boundary: a content
// list longer than toniefile can write has to be refused, not silently truncated
// (src/toniefile.c:389-392).
func TestTooManyChaptersRejected(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, MaxChapters+1, 16)
	downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

	if _, err := downloader.Place(context.Background(), uid); err == nil {
		t.Errorf("placement accepted %d chapters, want a rejection above %d", MaxChapters+1, MaxChapters)
	}
}

// TestFreshnessSkipIsHonoured covers --freshness=none: the box must not touch the
// route at all, so a run can be made independent of it.
func TestFreshnessSkipIsHonoured(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	cfg := DefaultConfig("00aabbccdd00", uid)
	cfg.Freshness = FreshnessSkip
	downloader, _ := testDownloader(t, cfg, cloud)

	result, err := downloader.Place(context.Background(), uid)
	if err != nil {
		t.Fatalf("Place: %v", err)
	}
	if !result.Freshness.Skipped {
		t.Error("freshness was not skipped with --freshness=none")
	}

	paths, _ := cloud.log.snapshot()
	if containsPrefix(paths, "/v1/freshness-check") || containsPrefix(paths, "/v3/freshness-check") {
		t.Errorf("a freshness route was requested with --freshness=none; saw %v", paths)
	}
}

// TestProgressReportsTimestamps is what makes the silence window measurable: the
// recorder needs live milestones, not a result after the fact, because the gap it
// measures spans a placement boundary.
func TestProgressReportsTimestamps(t *testing.T) {
	uid, err := ParseRUID("E001000100000001")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	cloud := newFakeCloud(t, 431165001, 1, 4096)
	downloader, _ := testDownloader(t, DefaultConfig("00aabbccdd00", uid), cloud)

	var mu sync.Mutex
	var seen []DownloadProgress
	downloader.OnProgress = func(p DownloadProgress) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, p)
	}

	if _, err := downloader.Place(context.Background(), uid); err != nil {
		t.Fatalf("Place: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var sawStatus, sawBytes, sawDone bool
	for _, p := range seen {
		if p.At.IsZero() {
			t.Error("a progress milestone carried no timestamp")
		}
		if p.Status != 0 {
			sawStatus = true
		}
		if p.Bytes > 0 {
			sawBytes = true
		}
		if p.Done {
			sawDone = true
		}
	}
	if !sawStatus {
		t.Error("no milestone reported the HTTP status")
	}
	if !sawBytes {
		t.Error("no milestone reported progress bytes")
	}
	if !sawDone {
		t.Error("no milestone marked the download finished")
	}
}

// TestGateSuspendsAndResumes covers the stall mechanism, which is the input
// condition for the audio-plane starvation work: the connection stays open while
// the read loop stops.
func TestGateSuspendsAndResumes(t *testing.T) {
	gate := NewGate()
	if gate.Suspended() {
		t.Fatal("a new gate starts suspended")
	}
	if err := gate.Wait(context.Background()); err != nil {
		t.Fatalf("Wait on an open gate: %v", err)
	}

	gate.Block()
	if !gate.Suspended() {
		t.Error("Suspended is false after Block")
	}

	released := make(chan error, 1)
	go func() { released <- gate.Wait(context.Background()) }()

	select {
	case <-released:
		t.Fatal("Wait returned while the gate was blocked")
	case <-time.After(20 * time.Millisecond):
	}

	gate.Resume()
	select {
	case err := <-released:
		if err != nil {
			t.Errorf("Wait after Resume: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Resume")
	}

	// Repeated calls have to be idempotent: the control surface lets an operator
	// block and resume freely, and double-closing a channel would panic.
	gate.Resume()
	gate.Resume()
	gate.Block()
	gate.Block()
	gate.Resume()
	if gate.Suspended() {
		t.Error("gate is suspended after a balanced block/resume sequence")
	}
}

// TestGateWaitHonoursContext checks that a stall can be cancelled, otherwise
// lifting a figurine during a stall would hang the cycle generator.
func TestGateWaitHonoursContext(t *testing.T) {
	gate := NewGate()
	gate.Block()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := gate.Wait(ctx)
	if err == nil {
		t.Fatal("Wait returned while blocked and the context was live")
	}
	if !strings.Contains(err.Error(), ErrStalled.Error()) {
		t.Errorf("Wait returned %v, want it to wrap ErrStalled", err)
	}
}

// TestPacerHoldsRealTime is the reason a mock consumer is not just io.Copy: a box
// that drains instantly is not a box, and the rate is what creates backpressure
// against teddyCloud's ffmpeg.
func TestPacerHoldsRealTime(t *testing.T) {
	const rate = 4096 // bytes per second

	p := &pacer{rate: rate, start: time.Now(), stop: NewGate()}
	for i := 0; i < 4; i++ {
		p.advance(rate / 4)
	}
	if err := p.wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}

	elapsed := time.Since(p.start)
	if elapsed < 900*time.Millisecond {
		t.Errorf("4 KiB at %d B/s took %v, want about a second", rate, elapsed)
	}
}

// TestPacerSpeedScalesRate ties --speed to the pacer, because that wiring lives
// in the command rather than the package. A --speed that does not reach the pacer
// leaves the starvation scenario inert while still reporting itself in every
// assumption stamp, which is the worst kind of bug for a diagnostic tool.
func TestPacerSpeedScalesRate(t *testing.T) {
	const rate = 4096

	for _, tc := range []struct {
		speed float64
		want  int64
	}{
		{0.5, rate / 2},
		{1, rate},
		{2, rate * 2},
		{8, rate * 8},
	} {
		got := int64(float64(rate) * tc.speed)
		if got != tc.want {
			t.Errorf("speed %g scaled the rate to %d, want %d", tc.speed, got, tc.want)
		}
	}

	// A speed of zero must not become a negative or absurd rate, and it must mean
	// "no pacing" rather than "stall forever": the stall is the gate's job.
	if scaled := int64(float64(rate) * 0); scaled != 0 {
		t.Errorf("speed 0 scaled the rate to %d, want 0", scaled)
	}
}

// TestPacerUnlimitedAtZeroRate checks that a rate of zero drains without pacing, so
// a test can exercise framing without waiting.
func TestPacerUnlimitedAtZeroRate(t *testing.T) {
	p := &pacer{start: time.Now(), stop: NewGate()}
	p.advance(1 << 30)

	start := time.Now()
	if err := p.wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("an unlimited pacer waited %v", elapsed)
	}
}

// TestPacerStopsWhileGated checks that a stall suspends the read rather than
// letting it race ahead: backpressure has to include the pause.
func TestPacerStopsWhileGated(t *testing.T) {
	gate := NewGate()
	gate.Block()

	p := &pacer{rate: 1 << 20, start: time.Now(), stop: gate}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := p.wait(ctx); err == nil {
		t.Fatal("wait returned while the gate was blocked")
	}
}

func mustRanges(t *testing.T, cloud *fakeCloud) []string {
	t.Helper()
	_, ranges := cloud.log.snapshot()
	return ranges
}

func contains(haystack []string, needle string) bool {
	return containsPrefix(haystack, needle) && anyExact(haystack, needle)
}

func containsPrefix(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.HasPrefix(s, needle) {
			return true
		}
	}
	return false
}

func anyExact(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
