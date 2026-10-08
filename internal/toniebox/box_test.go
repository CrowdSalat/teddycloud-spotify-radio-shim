package toniebox

import (
	"bytes"
	"testing"
	"time"
)

// rpcParts unwraps a frame into its log2 and log3 messages. Only those two
// fields exist on TonieRtnlRPC, so anything else is an encoder bug.
func rpcParts(t *testing.T, frame []byte) (log2, log3 []rawField) {
	t.Helper()

	for _, f := range walk(t, splitFrame(t, frame)) {
		switch f.field {
		case 2:
			log2 = walk(t, f.val)
		case 3:
			log3 = walk(t, f.val)
		default:
			t.Fatalf("RPC carries unexpected field %d", f.field)
		}
	}
	return log2, log3
}

func num(t *testing.T, fields []rawField, want int) uint64 {
	t.Helper()
	return find(t, fields, want).num
}

func bytesOf(t *testing.T, fields []rawField, want int) []byte {
	t.Helper()
	return find(t, fields, want).val
}

// ruidByte decodes hex nibble pair i of a 16-character rUID.
func ruidByte(ruid string, i int) byte {
	return byte(hexNibble(ruid[i*2])<<4 | hexNibble(ruid[i*2+1]))
}

func hexNibble(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

// TestRUIDRoundTrip is the check that keeps content URLs resolvable. The rUID
// and the uid are byte-reversed relative to each other
// (src/handler.c:647-658, src/handler_cloud.c:489-490), and getting it wrong
// yields a 404 for a figurine that is perfectly present.
func TestRUIDRoundTrip(t *testing.T) {
	for _, ruid := range []string{
		"E00403500EEA4BF2",
		"FA9C3E0D500304E0",
		"0000000000000000",
		"FFFFFFFFFFFFFFFF",
	} {
		uid, err := ParseRUID(ruid)
		if err != nil {
			t.Errorf("ParseRUID(%q): %v", ruid, err)
			continue
		}
		if got := RUIDHex(uid); got != ruid {
			t.Errorf("RUIDHex(ParseRUID(%q)) = %q", ruid, got)
		}

		// field6 carries the uid big-endian, so its bytes are the rUID's in
		// reverse; that is what teddyCloud compares against the echo request's
		// echoed bytes (src/handler_rtnl.c:336-350).
		field6 := UIDBytes(uid)
		for i := 0; i < 8; i++ {
			if field6[i] != ruidByte(ruid, 7-i) {
				t.Errorf("UIDBytes(%q)[%d] = %02X, want %02X", ruid, i, field6[i], ruidByte(ruid, 7-i))
			}
		}
	}
}

// TestUIDBytesAreTheReversedRUID states the relationship directly rather than
// only round-tripping it, so a bug that reverses both directions at once still
// fails.
func TestUIDBytesAreTheReversedRUID(t *testing.T) {
	const ruid = "E00403500EEA4BF2"
	uid, err := ParseRUID(ruid)
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	got := UIDBytes(uid)
	want := []byte{0xF2, 0x4B, 0xEA, 0x0E, 0x50, 0x03, 0x04, 0xE0}
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] ||
		got[4] != want[4] || got[5] != want[5] || got[6] != want[6] || got[7] != want[7] {
		t.Errorf("UIDBytes = % X, want % X", got, want)
	}
}

// TestTagFrames checks the frame order a placement produces: the
// playback-starting transition first, then the tag frame. teddyCloud derives
// nothing from the order, but a capture that disagrees with the mock is a sign
// the mock has drifted from the hardware it is standing in for.
func TestTagFrames(t *testing.T) {
	uid, err := ParseRUID("E00403500EEA4BF2")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	valid, err := NewBox(DefaultConfig("00aabbccdd00", uid)).TagFrames(TagValid, uid)
	if err != nil {
		t.Fatalf("TagFrames: %v", err)
	}
	if len(valid) != 2 {
		t.Fatalf("valid placement produced %d frames, want 2", len(valid))
	}

	_, starting := rpcParts(t, valid[0])
	if len(starting) == 0 {
		t.Fatal("first frame carries no log3")
	}
	if got := num(t, starting, 2); got != uint64(Log3PlaybackStarting) {
		t.Errorf("first log3 field2 = %d, want %d", got, Log3PlaybackStarting)
	}

	log2, _ := rpcParts(t, valid[1])
	if got := num(t, log2, 4); got != uint64(GroupTag) {
		t.Errorf("tag frame group = %d, want %d", got, GroupTag)
	}
	if got := num(t, log2, 5); got != 8627 {
		t.Errorf("tag frame function = %d, want 8627", got)
	}
	if got := bytesOf(t, log2, 6); string(got) != string(UIDBytes(uid)) {
		t.Errorf("tag frame field6 = % X, want % X", got, UIDBytes(uid))
	}

	removed, err := NewBox(DefaultConfig("00aabbccdd00", uid)).TagFrames(TagRemoved, uid)
	if err != nil {
		t.Fatalf("TagFrames removed: %v", err)
	}
	if len(removed) != 1 {
		t.Fatalf("removal produced %d frames, want 1", len(removed))
	}
	if _, l3 := rpcParts(t, removed[0]); len(l3) != 0 {
		t.Error("removal emitted a log3 frame; teddyCloud would show a playback event")
	}
	remLog2, _ := rpcParts(t, removed[0])
	if got := num(t, remLog2, 5); got != 8630 {
		t.Errorf("removal function = %d, want 8630", got)
	}
}

// TestPlaybackStopFrames checks that a lift reports its volume before the stop
// and that the stop clears tag state — which is why every cycle has to re-send
// the placement frame rather than reusing the previous one.
func TestPlaybackStopFrames(t *testing.T) {
	box := NewBox(DefaultConfig("00aabbccdd00", 1))

	frames, err := box.PlaybackStopFrames(12, -3)
	if err != nil {
		t.Fatalf("PlaybackStopFrames: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("stop produced %d frames, want 2", len(frames))
	}

	volLog2, _ := rpcParts(t, frames[0])
	if got := num(t, volLog2, 4); got != uint64(GroupVolume) {
		t.Errorf("first frame group = %d, want %d", got, GroupVolume)
	}
	_, stopped := rpcParts(t, frames[1])
	if got := num(t, stopped, 2); got != uint64(Log3PlaybackStopped) {
		t.Errorf("second log3 field2 = %d, want %d", got, Log3PlaybackStopped)
	}
}

// TestEarPressSequences is the double-press trap made executable. A single
// press is two frames; a double needs four, because teddyCloud only ever sets
// its window flag from a volume frame (src/handler_rtnl.c:433-437) and never
// from the press itself.
func TestEarPressSequences(t *testing.T) {
	cfg := DefaultConfig("00aabbccdd00", 1)
	cfg.DoublePressMS = 5 // structural check, not a timing one

	box := NewBox(cfg)

	single, err := box.EarPressFrames(Log3EarBig, false, 12, -3)
	if err != nil {
		t.Fatalf("single ear press: %v", err)
	}
	if len(single) != 2 {
		t.Fatalf("single press produced %d frames, want 2", len(single))
	}
	assertEarOrder(t, single, false)

	double, err := box.EarPressFrames(Log3EarBig, true, 12, -3)
	if err != nil {
		t.Fatalf("double ear press: %v", err)
	}
	if len(double) != 4 {
		t.Fatalf("double press produced %d frames, want 4", len(double))
	}
	assertEarOrder(t, double, true)
}

func assertEarOrder(t *testing.T, frames [][]byte, double bool) {
	t.Helper()

	want := 2
	if double {
		want = 4
	}
	for i, frame := range frames[:want] {
		log2, log3 := rpcParts(t, frame)
		if i%2 == 0 {
			if len(log3) != 0 {
				t.Fatalf("frame %d carries a log3, want the volume frame", i)
			}
			if got := num(t, log2, 4); got != uint64(GroupVolume) {
				t.Fatalf("frame %d group = %d, want %d", i, got, GroupVolume)
			}
			continue
		}
		if len(log2) != 0 {
			t.Fatalf("frame %d carries a log2, want the press", i)
		}
		if got := num(t, log3, 2); got != uint64(Log3EarBig) {
			t.Fatalf("frame %d log3 field2 = %d, want %d", i, got, Log3EarBig)
		}
	}
}

// TestVolumeFrameSpacing pins the guard that keeps an ordinary press ordinary:
// the mock's own volume frames have to stay outside teddyCloud's window, or the
// flag is armed and a same-ear press is reported as a double. Nothing ever
// clears that flag except a successful double, so getting it wrong is sticky.
func TestVolumeFrameSpacing(t *testing.T) {
	box := NewBox(DefaultConfig("00aabbccdd00", 1))

	first := time.Now()
	if _, err := box.volumeFrame(12, -3, true); err != nil {
		t.Fatalf("volumeFrame: %v", err)
	}
	if _, err := box.volumeFrame(12, -3, true); err != nil {
		t.Fatalf("volumeFrame: %v", err)
	}
	elapsed := time.Since(first)

	if elapsed < MultiPressWindow {
		t.Errorf("two spaced volume frames took %v, want at least %v", elapsed, MultiPressWindow)
	}

	// The unspaced variant must not wait, since that is what a double's second
	// frame relies on.
	first = time.Now()
	if _, err := box.volumeFrame(12, -3, false); err != nil {
		t.Fatalf("volumeFrame: %v", err)
	}
	if _, err := box.volumeFrame(12, -3, false); err != nil {
		t.Fatalf("volumeFrame: %v", err)
	}
	if elapsed := time.Since(first); elapsed >= MultiPressWindow {
		t.Errorf("two unspaced volume frames took %v, want less than %v", elapsed, MultiPressWindow)
	}
}

// TestAssumptionsAreStamped is what makes a report attributable: every flag that
// changes behaviour has to appear, or a measurement cannot be traced back to the
// configuration that produced it.
func TestAssumptionsAreStamped(t *testing.T) {
	cfg := DefaultConfig("00aabbccdd00", 1)
	cfg.PadField6 = 32
	cfg.DropAfter = 4096

	got := map[string]string{}
	for _, pair := range cfg.Assumptions() {
		got[pair[0]] = pair[1]
	}

	for _, want := range []string{
		"firmware", "mac", "uid", "content_path", "fallback", "freshness",
		"report_audio_id", "recheck", "range_offset", "speed", "stall_ms",
		"drop_after_bytes", "max_download_bytes", "double_press_ms", "pad_field6_bytes",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("Assumptions() is missing %q", want)
		}
	}
	if got["pad_field6_bytes"] != "32" {
		t.Errorf("pad_field6_bytes = %q, want 32", got["pad_field6_bytes"])
	}
	if got["drop_after_bytes"] != "4096" {
		t.Errorf("drop_after_bytes = %q, want 4096", got["drop_after_bytes"])
	}
}

// TestTelemetryFrames checks the informational frames as a set rather than one
// by one, because what matters about them is that teddyCloud can route every one
// of them to a settings key without reading off the end of its buffer. The
// payload-level guarantees live in TestStringFramesAreNULTerminated; this pins
// which (group, function) pairs are sent, since a frame on the wrong pair is
// silently ignored instead of rejected.
//
// These are also the frames that used to crash teddyCloud, which is why they are
// covered rather than assumed.
func TestTelemetryFrames(t *testing.T) {
	frames, err := NewBox(DefaultConfig("00aabbccdd00", 1)).TelemetryFrames()
	if err != nil {
		t.Fatalf("TelemetryFrames: %v", err)
	}

	type pair struct{ group, function uint32 }
	want := []pair{
		{GroupFirmware, FirmwareRTNLVersion},
		{GroupFirmware, FirmwareRTNLFullVersion},
		{GroupNetworkHTTP, NetworkRegion},
		{GroupNetworkHTTP, NetworkProdDomain},
		{GroupNetworkTCP, NetworkRTNLDomain},
		{GroupFirmware, FirmwareRTNLDetail},
	}

	if len(frames) != len(want) {
		t.Fatalf("telemetry produced %d frames, want %d", len(frames), len(want))
	}

	for i, frame := range frames {
		log2, log3 := rpcParts(t, frame)
		if len(log3) != 0 {
			t.Errorf("frame %d carries a log3; telemetry is log2 only", i)
		}
		if got := (pair{uint32(num(t, log2, 4)), uint32(num(t, log2, 5))}); got != want[i] {
			t.Errorf("frame %d is group %d function %d, want group %d function %d",
				i, got.group, got.function, want[i].group, want[i].function)
		}

		// The value must be present, not merely NUL-terminated.
		field6 := bytesOf(t, log2, 6)
		if len(bytes.Trim(field6, "\x00")) == 0 {
			t.Errorf("frame %d carries no telemetry value", i)
		}
	}
}

// TestFreshnessInfosOnEmptyCache covers the first placement of every run.
//
// An empty cache reports audio id 0 and still performs the exchange. Reporting
// nothing would look tidier but silently skips /v1/freshness-check for the whole
// first cycle, which is precisely when a run is least able to notice: the
// freshness routes go unexercised and the box never learns to report an id.
func TestFreshnessInfosOnEmptyCache(t *testing.T) {
	for _, policy := range []ReportAudioID{ReportTAF, ReportZero, ReportStale} {
		t.Run(policy.String(), func(t *testing.T) {
			cfg := DefaultConfig("00aabbccdd00", 1)
			cfg.Report = policy

			infos, err := NewBox(cfg).FreshnessInfos()
			if err != nil {
				t.Fatalf("FreshnessInfos: %v", err)
			}
			if len(infos) != 1 {
				t.Fatalf("got %d infos, want 1: an empty cache still has to be reported", len(infos))
			}
			if infos[0].AudioID != 0 {
				t.Errorf("audio id = %d on an empty cache, want 0", infos[0].AudioID)
			}
		})
	}
}

// TestReportAudioIDPolicies pins the four policies. ReportOmit is the only one
// that withholds the body, because withholding it is what it exists to do.
func TestReportAudioIDPolicies(t *testing.T) {
	const cached = 431165001

	cases := []struct {
		policy ReportAudioID
		want   uint32
		ok     bool
	}{
		{ReportTAF, cached, true},
		{ReportZero, 0, true},
		{ReportStale, cached - 1, true},
		{ReportOmit, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.policy.String(), func(t *testing.T) {
			cfg := DefaultConfig("00aabbccdd00", 1)
			cfg.Report = tc.policy

			box := NewBox(cfg)
			box.NoteDownloadedTAF(cached)

			got, ok := box.CachedAudioID()
			if ok != tc.ok {
				t.Errorf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("audio id = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFreshnessInfosUsePlacedUID checks that the box reports the tag it is
// actually carrying rather than the configured default, since after a lift the
// two differ and reporting the wrong one asks teddyCloud about a figurine that is
// not on the box.
func TestFreshnessInfosUsePlacedUID(t *testing.T) {
	other, err := ParseRUID("FA9C3E0D500304E0")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	box := NewBox(DefaultConfig("00aabbccdd00", 1))
	if _, err := box.TagFrames(TagValid, other); err != nil {
		t.Fatalf("TagFrames: %v", err)
	}

	infos, err := box.FreshnessInfos()
	if err != nil {
		t.Fatalf("FreshnessInfos: %v", err)
	}
	if len(infos) != 1 || infos[0].UID != other {
		t.Fatalf("reported uid = %016X, want %016X", infos[0].UID, other)
	}
}

// TestUptimeNeverGoesBackwards guards the double-press arithmetic: the window
// check is an unsigned subtraction (src/handler_rtnl.c:433-437), so an uptime
// that moves backwards turns an ancient press into one that just happened.
// Millisecond granularity means the value may hold still; it must never retreat.
func TestUptimeNeverGoesBackwards(t *testing.T) {
	box := NewBox(DefaultConfig("00aabbccdd00", 1))

	last := box.Uptime()
	for i := 0; i < 5; i++ {
		time.Sleep(time.Millisecond)
		got := box.Uptime()
		if got < last {
			t.Fatalf("uptime went from %d to %d", last, got)
		}
		last = got
	}
	if last < 5 {
		t.Errorf("uptime = %d after ~5ms, want at least 5", last)
	}
}
