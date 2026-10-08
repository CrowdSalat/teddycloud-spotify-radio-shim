package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crowdsalat/teddycloud-spotify-radio-shim/internal/toniebox"
)

// at builds a timestamp relative to a fixed base, so the timing assertions do
// not depend on wall-clock drift between when a test writes a cycle and when it
// inspects the measurement.
func at(base time.Time, ms int) time.Time {
	return base.Add(time.Duration(ms) * time.Millisecond)
}

// newTestRecorder returns a recorder plus the base timestamp its cycles should be
// laid out around.
func newTestRecorder(t *testing.T) (*recorder, time.Time) {
	t.Helper()

	cfg := toniebox.DefaultConfig("00aabbccdd00", 0x7205759405470563)
	rec := newRecorder(cfg, "00aabbccdd00")
	return rec, time.Now()
}

// playCycle writes the placement half of a cycle: the events a placement produces
// and the download milestones, without the lift.
func playCycle(rec *recorder, base time.Time, n int, firstByteMS, lastByteMS int, audioID uint32) {
	rec.begin(n, 0x7205759405470563)
	rec.note(observedEvent{At: at(base, firstByteMS/2), Name: "TagValid", Data: `{"type":"TagValid","data":"E001000100000001"}`})
	rec.note(observedEvent{At: at(base, firstByteMS/2), Name: "playback", Data: `{"type":"playback","data":"started"}`})
	rec.progress(toniebox.DownloadProgress{
		At: at(base, firstByteMS), Path: "/v3/chapter/x",
		Status: 200, Advertised: 40960, Bytes: 1024,
	})
	rec.progress(toniebox.DownloadProgress{
		At: at(base, lastByteMS), Path: "/v3/chapter/x",
		Status: 200, Advertised: 40960, Bytes: 40960, AudioID: audioID,
		HasTAF: true, Chapters: 1, Done: true,
	})
	rec.finish()
}

// recordCycle is a complete cycle: a placement followed by the lift that ends it.
//
// The stop is noted after finish() because that is the order a real run produces —
// playback=stopped belongs to no cycle, it ends one — and because recording it
// inside the cycle would attribute it to the placement that follows.
func recordCycle(rec *recorder, base time.Time, n int, firstByteMS, lastByteMS int, audioID uint32) {
	playCycle(rec, base, n, firstByteMS, lastByteMS, audioID)
	rec.note(observedEvent{At: at(base, lastByteMS+10), Name: "playback", Data: `{"type":"playback","data":"stopped"}`})
}

// recordCycleNoStop is a placement that is never lifted, so no playback=stopped
// ever arrives for it. It stands for both "the last figurine was left on the box"
// and "the observer never saw the transitions": in either case the stop count is
// short, and the assertion is the same.
func recordCycleNoStop(rec *recorder, base time.Time, n int, firstByteMS, lastByteMS int, audioID uint32) {
	playCycle(rec, base, n, firstByteMS, lastByteMS, audioID)
}

// findMeasurement returns the named measurement, or nil.
func findMeasurement(ms []measurement, name string) *measurement {
	for i := range ms {
		if ms[i].Name == name {
			return &ms[i]
		}
	}
	return nil
}

// TestReportStampsAssumptions is the property that makes a report attributable at
// all. A measurement is meaningless without the box behaviour that produced it,
// so the assumption set has to lead the document.
func TestReportStampsAssumptions(t *testing.T) {
	cfg := toniebox.DefaultConfig("00aabbccdd00", 1)
	cfg.Firmware = toniebox.ESP32
	cfg.Range = toniebox.RangeResume
	cfg.DropAfter = 4096

	rec := newRecorder(cfg, "00aabbccdd00")
	raw := rec.report(nil)

	assumptions, ok := raw["assumptions"]
	if !ok {
		t.Fatal("report carries no assumptions")
	}
	list, ok := assumptions.([][2]string)
	if !ok {
		t.Fatalf("assumptions is %T, want [][2]string", assumptions)
	}

	got := map[string]string{}
	for _, pair := range list {
		got[pair[0]] = pair[1]
	}
	for key, want := range map[string]string{
		"firmware":         "esp32",
		"range_offset":     "resume",
		"drop_after_bytes": "4096",
	} {
		if got[key] != want {
			t.Errorf("assumption %q = %q, want %q", key, got[key], want)
		}
	}

	if raw["overlay_id"] != "00aabbccdd00" {
		t.Errorf("overlay_id = %v, want the box MAC", raw["overlay_id"])
	}

	// The serialised form has to survive a round trip, since that is what lands
	// in the artefact file.
	if _, err := json.Marshal(raw); err != nil {
		t.Errorf("report is not serialisable: %v", err)
	}
}

// TestReportCountsCycles checks the basic tally and that an empty run reports a
// failure rather than a vacuous pass.
func TestReportCountsCycles(t *testing.T) {
	rec, base := newTestRecorder(t)
	recordCycle(rec, base, 1, 100, 500, 431165001)
	recordCycle(rec, base, 2, 4000, 4400, 431165001)

	cycles, _ := rec.snapshot()
	ms := rec.measurements(cycles, nil, nil)

	cycles_ := findMeasurement(ms, "cycles")
	if cycles_ == nil {
		t.Fatal("no cycles measurement")
	}
	if cycles_.Value != "2" {
		t.Errorf("cycles = %q, want 2", cycles_.Value)
	}
	if cycles_.Pass == nil || !*cycles_.Pass {
		t.Error("two recorded cycles did not pass")
	}

	empty := rec.measurements(nil, nil, nil)
	if c := findMeasurement(empty, "cycles"); c.Pass == nil || *c.Pass {
		t.Error("a run with no cycles reported a pass")
	}
}

// TestReportDetectsRepeatedAudioID is the ContentAudioId invariant: a re-placement
// of unchanged content must not produce a new audio id. This is the check that
// catches a re-render leaking through.
func TestReportDetectsRepeatedAudioID(t *testing.T) {
	rec, base := newTestRecorder(t)
	recordCycle(rec, base, 1, 100, 500, 431165001)
	recordCycle(rec, base, 2, 4000, 4400, 999888777) // a new id for the same content

	cycles, _ := rec.snapshot()
	// The recorder collected the ids itself; pass them through as it would.
	ms := rec.measurements(cycles, nil, collectedAudioIDs(rec))

	ids := findMeasurement(ms, "content_audio_ids")
	if ids == nil {
		t.Fatal("no content_audio_ids measurement")
	}
	if ids.Pass == nil || *ids.Pass {
		t.Errorf("two audio ids for one content source passed: %s", ids.Value)
	}

	// And the healthy case.
	rec2, base2 := newTestRecorder(t)
	recordCycle(rec2, base2, 1, 100, 500, 431165001)
	recordCycle(rec2, base2, 2, 4000, 4400, 431165001)
	cycles2, _ := rec2.snapshot()
	ms2 := rec2.measurements(cycles2, nil, collectedAudioIDs(rec2))
	if ids := findMeasurement(ms2, "content_audio_ids"); ids.Pass == nil || !*ids.Pass {
		t.Errorf("a stable audio id failed: %s", ids.Value)
	}
}

// collectedAudioIDs reads back the distinct ids the recorder filed, which is what
// report() would stamp.
func collectedAudioIDs(rec *recorder) []uint32 {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return append([]uint32(nil), rec.audioIDs...)
}

// TestReportCountsStoppedPerCycle covers the pause-versus-latch confusion: a lift
// must produce exactly one playback=stopped, and nothing else may produce one.
func TestReportCountsStoppedPerCycle(t *testing.T) {
	cases := []struct {
		name  string
		build func(rec *recorder, base time.Time)
		want  string
		pass  bool
	}{
		{
			name: "one stop per cycle",
			build: func(rec *recorder, base time.Time) {
				recordCycle(rec, base, 1, 100, 500, 7)
				recordCycle(rec, base, 2, 4000, 4400, 7)
			},
			want: "1.00", pass: true,
		},
		{
			// The second figurine is placed but never lifted, so teddyCloud is
			// still playing when the run ends and only one stop arrives.
			name: "a placement that is never lifted",
			build: func(rec *recorder, base time.Time) {
				recordCycle(rec, base, 1, 100, 500, 7)
				recordCycleNoStop(rec, base, 2, 4000, 4400, 7)
			},
			want: "0.50", pass: false,
		},
		{
			// The lift produced two stops, which is the latch half of the
			// pause-versus-latch confusion.
			name: "a lift that produced two stops",
			build: func(rec *recorder, base time.Time) {
				recordCycle(rec, base, 1, 100, 500, 7)
				rec.note(observedEvent{At: at(base, 510), Name: "playback", Data: `{"type":"playback","data":"stopped"}`})
			},
			want: "2.00", pass: false,
		},
		{
			// No lift and no stop: the run ended with the figurine still playing.
			name: "no playback stop observed",
			build: func(rec *recorder, base time.Time) {
				recordCycleNoStop(rec, base, 1, 100, 500, 7)
			},
			want: "0.00", pass: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, base := newTestRecorder(t)
			tc.build(rec, base)

			cycles, events := rec.snapshot()
			ms := rec.measurements(cycles, events, nil)

			got := findMeasurement(ms, "stopped_per_cycle")
			if got == nil {
				t.Fatal("no stopped_per_cycle measurement")
			}
			if got.Value != tc.want {
				t.Errorf("stopped_per_cycle = %q, want %q", got.Value, tc.want)
			}
			if got.Pass == nil || *got.Pass != tc.pass {
				t.Errorf("pass = %v, want %v", got.Pass, tc.pass)
			}
		})
	}
}

// TestReportMeasuresTheSilenceWindow is the number the tool exists to produce: the
// gap between one placement's last byte and the next one's first. Issue 1 is a
// 10-20 s silence after a re-placement, so this has to be measured and not merely
// asserted.
func TestReportMeasuresTheSilenceWindow(t *testing.T) {
	rec, base := newTestRecorder(t)
	// Last byte of cycle 1 at t=500ms; first byte of cycle 2 at t=15000ms:
	// a 14.5 s hole, the shape of the reported defect.
	recordCycle(rec, base, 1, 100, 500, 7)
	recordCycle(rec, base, 2, 15000, 15400, 7)

	cycles, _ := rec.snapshot()
	ms := rec.measurements(cycles, nil, nil)

	mean := findMeasurement(ms, "inter_cycle_gap_mean_ms")
	if mean == nil {
		t.Fatal("no inter_cycle_gap_mean_ms measurement")
	}
	if mean.Value != "14500" {
		t.Errorf("mean gap = %q ms, want 14500", mean.Value)
	}

	maxGap := findMeasurement(ms, "inter_cycle_gap_max_ms")
	if maxGap == nil {
		t.Fatal("no inter_cycle_gap_max_ms measurement")
	}
	if maxGap.Value != "14500" {
		t.Errorf("max gap = %q ms, want 14500", maxGap.Value)
	}

	// A single cycle has no gap to measure, so it must report none rather than a
	// misleading zero.
	rec2, base2 := newTestRecorder(t)
	recordCycle(rec2, base2, 1, 100, 500, 7)
	cycles2, _ := rec2.snapshot()
	ms2 := rec2.measurements(cycles2, nil, nil)
	if findMeasurement(ms2, "inter_cycle_gap_mean_ms") != nil {
		t.Error("a single cycle reported an inter-cycle gap")
	}
}

// TestReportFlagsEmptyCycles checks that a placement which produced no bytes is a
// failure. Silence with no stream is exactly the bug being looked for.
// TestProgressRecordsTheRouteUsed checks the recorded content path names the route
// the bytes arrived on rather than the configured one. Getting this wrong reports
// a placement served by a v2 fallback as v3, which sends whoever reads the report
// looking in the wrong place — and it is invisible when no fallback fires.
func TestProgressRecordsTheRouteUsed(t *testing.T) {
	cases := []struct {
		name, path, want string
	}{
		{"v3 chapter", "/v3/chapter/teddycloud_00_e001000100000001", "v3"},
		{"v2 content", "/v2/content/E001000100000001", "v2"},
		{"v3 meta is not a content route", "/v3/content-meta/E001000100000001", ""},
		{"empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, _ := newTestRecorder(t)
			rec.begin(1, 0x7205759405470563)
			rec.progress(toniebox.DownloadProgress{
				At: time.Now(), Path: tc.path, Status: 200, Bytes: 4096, Done: true,
			})

			c := rec.finish()
			if c.Path != tc.want {
				t.Errorf("recorded path = %q, want %q", c.Path, tc.want)
			}
		})
	}
}

// TestProgressLastRouteWins covers a placement that fell back mid-way: the route
// that delivered the content is the one worth reporting.
func TestProgressLastRouteWins(t *testing.T) {
	rec, base := newTestRecorder(t)
	rec.begin(1, 0x7205759405470563)
	rec.progress(toniebox.DownloadProgress{
		At: at(base, 10), Path: "/v3/content-meta/E001000100000001", Status: 404,
	})
	rec.progress(toniebox.DownloadProgress{
		At: at(base, 20), Path: "/v2/content/E001000100000001", Status: 200, Bytes: 4096, Done: true,
	})

	if c := rec.finish(); c.Path != "v2" {
		t.Errorf("recorded path = %q, want v2", c.Path)
	}
}

func TestReportFlagsEmptyCycles(t *testing.T) {
	rec, base := newTestRecorder(t)
	recordCycle(rec, base, 1, 100, 500, 7)
	rec.begin(2, 1) // begun, never delivered a byte
	rec.finish()

	cycles, _ := rec.snapshot()
	ms := rec.measurements(cycles, nil, nil)

	withContent := findMeasurement(ms, "cycles_with_content")
	if withContent == nil {
		t.Fatal("no cycles_with_content measurement")
	}
	if withContent.Value != "1" {
		t.Errorf("cycles_with_content = %q, want 1", withContent.Value)
	}
	if withContent.Pass == nil || *withContent.Pass {
		t.Error("a cycle with no bytes passed")
	}
}

// TestReportFlagsFailedCycles and TestReportFlagsDistinctUIDs cover the remaining
// per-cycle invariants: a placement that errored, and a run whose cycles disagree
// about which figurine was on the box.
func TestReportFlagsFailedCycles(t *testing.T) {
	rec, base := newTestRecorder(t)
	recordCycle(rec, base, 1, 100, 500, 7)
	rec.begin(2, 1)
	rec.progress(toniebox.DownloadProgress{
		At: base, Path: "/v3/chapter/x", Status: 500, Done: true,
		Err: errFake{},
	})
	rec.finish()

	cycles, _ := rec.snapshot()
	ms := rec.measurements(cycles, nil, nil)

	failed := findMeasurement(ms, "failed_cycles")
	if failed == nil {
		t.Fatal("no failed_cycles measurement")
	}
	if failed.Value != "1" {
		t.Errorf("failed_cycles = %q, want 1", failed.Value)
	}
	if failed.Pass == nil || *failed.Pass {
		t.Error("a failed cycle passed")
	}
}

type errFake struct{}

func (errFake) Error() string { return "injected failure" }

func TestReportFlagsDistinctUIDs(t *testing.T) {
	rec, base := newTestRecorder(t)
	recordCycle(rec, base, 1, 100, 500, 7)

	// A second cycle on a different figurine.
	rec.begin(2, 0xFA9C3E0D500304E0)
	rec.progress(toniebox.DownloadProgress{
		At: at(base, 4000), Path: "/v3/chapter/x", Status: 200, Bytes: 2048, Done: true,
	})
	rec.finish()

	cycles, _ := rec.snapshot()
	ms := rec.measurements(cycles, nil, nil)

	distinct := findMeasurement(ms, "distinct_ruids")
	if distinct == nil {
		t.Fatal("no distinct_ruids measurement")
	}
	if distinct.Value != "2" {
		t.Errorf("distinct_ruids = %q, want 2", distinct.Value)
	}
	if distinct.Pass == nil || *distinct.Pass {
		t.Error("two figurines in one run passed a single-figurine assertion")
	}
}

// TestReportIgnoresEventsOutsideACycle keeps a placement that is not part of a run
// — an operator pressing the ear once by hand — from polluting the measurements
// of the cycle currently in flight.
func TestReportIgnoresEventsOutsideACycle(t *testing.T) {
	rec, base := newTestRecorder(t)
	rec.note(observedEvent{At: base, Name: "pressed", Data: `{"type":"pressed","data":"ear-big"}`})

	cycles, events := rec.snapshot()
	if len(events) != 1 {
		t.Fatalf("recorded %d events, want 1: the event log is not gated", len(events))
	}
	if len(cycles) != 0 {
		t.Errorf("recorded %d cycles, want none", len(cycles))
	}
}

// TestSSEParser covers the wire format teddyCloud publishes:
// `event:<name>` then `data: {"type":...,"data":...}`, terminated by a blank line
// (docs/research/teddycloud-sse-events.md).
func TestSSEParser(t *testing.T) {
	cases := []struct {
		name  string
		lines []string
		want  observedEvent
	}{
		{
			name:  "typed event",
			lines: []string{"event:TagValid", `data: {"type":"TagValid","data":"E001000100000001"}`, ""},
			want:  observedEvent{Name: "TagValid", Data: `{"type":"TagValid","data":"E001000100000001"}`},
		},
		{
			name:  "space after the colon",
			lines: []string{"event: playback", `data: {"type":"playback","data":"started"}`, ""},
			want:  observedEvent{Name: "playback", Data: `{"type":"playback","data":"started"}`},
		},
		{
			name:  "bare data line",
			lines: []string{"event:keep-alive", "data:", ""},
			want:  observedEvent{Name: "keep-alive", Data: ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p sseParser
			var got observedEvent
			var fired bool
			for _, line := range tc.lines {
				if evt, ok := p.feed(line); ok {
					got, fired = evt, true
				}
			}
			if !fired {
				t.Fatal("no event was produced")
			}
			if got.Name != tc.want.Name {
				t.Errorf("name = %q, want %q", got.Name, tc.want.Name)
			}
			if got.Data != tc.want.Data {
				t.Errorf("data = %q, want %q", got.Data, tc.want.Data)
			}
		})
	}
}

// TestSSEParserIgnoresCommentsAndBlanks covers the keep-alive comment and the
// blank lines around it, which appear constantly in a real feed.
func TestSSEParserIgnoresCommentsAndBlanks(t *testing.T) {
	var p sseParser
	for _, line := range []string{"", ": keep-alive", "", ""} {
		if evt, ok := p.feed(line); ok {
			t.Errorf("line %q produced an event: %+v", line, evt)
		}
	}
}

// TestObservedEventPayload covers unwrapping the inner data value, including the
// nested-object case that raw-log frames produce — 313 of 741 events in the live
// capture are raw-log, so getting this wrong loses the majority of the feed.
func TestObservedEventPayload(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"string payload", `{"type":"playback","data":"started"}`, "started"},
		{"nested object", `{"type":"rtnl-raw-log2","data":{"uptime":1,"function":2}}`, `{"uptime":1,"function":2}`},
		{"not json", `plain text`, "plain text"},
		{"no type", `{"data":"x"}`, `{"data":"x"}`},
		{"empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := observedEvent{Data: tc.data}.payload()
			if got != tc.want {
				t.Errorf("payload() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWriteReportIsStable covers the artefact: a diffable file needs stable
// formatting, a trailing newline, and no keys that move between two renderings of
// the same state other than the wall-clock elapsed time.
func TestWriteReportIsStable(t *testing.T) {
	rec, base := newTestRecorder(t)
	recordCycle(rec, base, 1, 100, 500, 7)

	cycles, _ := rec.snapshot()

	var first, second map[string]any
	for _, target := range []*map[string]any{&first, &second} {
		*target = map[string]any{}
		if err := json.Unmarshal(writeReport(rec.report(cycles)), target); err != nil {
			t.Fatalf("report is not valid JSON: %v", err)
		}
	}

	for _, key := range []string{"overlay_id", "assumptions", "measurements", "cycles", "events"} {
		a, _ := json.Marshal(first[key])
		b, _ := json.Marshal(second[key])
		if string(a) != string(b) {
			t.Errorf("%q differs between two renderings of the same state", key)
		}
	}

	raw := writeReport(rec.report(cycles))
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Error("report does not end with a newline")
	}

	for _, key := range []string{"overlay_id", "assumptions", "elapsed", "measurements", "cycles", "events"} {
		if _, ok := first[key]; !ok {
			t.Errorf("report is missing %q", key)
		}
	}
}

// TestSSEBaseURL covers the listener split: /api/sse is SERTY_WEB, so it lives on
// a different port from the box API and cannot share its base URL
// (src/server.c route table, src/server.c:915 and :935).
func TestSSEBaseURL(t *testing.T) {
	cases := []struct {
		name, tcBase, override, want string
	}{
		{"derived from https", "https://localhost:8443", "", "http://localhost:80"},
		{"derived from http", "http://teddycloud:8080", "", "http://teddycloud:80"},
		{"explicit override", "https://localhost:8443", "http://other:9000", "http://other:9000"},
		{"trailing slash trimmed", "https://localhost:8443", "http://other:9000/", "http://other:9000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sseBaseURL(tc.tcBase, tc.override); got != tc.want {
				t.Errorf("sseBaseURL(%q, %q) = %q, want %q", tc.tcBase, tc.override, got, tc.want)
			}
		})
	}
}

// TestParseCount covers the /control/cycle parameter, including the degenerate
// values that would otherwise start an unbounded load run.
func TestParseCount(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{"", 1},
		{"?count=5", 5},
		{"?count=200", 200},
		{"?count=0", 1},
		{"?count=-3", 1},
		{"?count=abc", 1},
	}

	for _, tc := range cases {
		t.Run("count="+tc.query, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/control/cycle"+tc.query, nil)
			if got := parseCount(r); got != tc.want {
				t.Errorf("parseCount(%q) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}
