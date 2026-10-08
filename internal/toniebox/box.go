package toniebox

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// ContentPath selects which of teddyCloud's two content routes the box uses.
// TB1 firmware is documented as /v1/* + /v2/content and TB2 as /v3/*
// (src/server.c:167-176); which one a real box actually prefers is not knowable
// from here, so it is a flag.
type ContentPath uint8

// Content paths.
const (
	ContentV2 ContentPath = iota
	ContentV3
)

func (p ContentPath) String() string {
	if p == ContentV3 {
		return "v3"
	}
	return "v2"
}

// ParseContentPath maps a flag value onto a ContentPath.
func ParseContentPath(s string) (ContentPath, error) {
	switch strings.ToLower(s) {
	case "v2":
		return ContentV2, nil
	case "v3":
		return ContentV3, nil
	default:
		return 0, fmt.Errorf("toniebox: unknown content path %q (want v2 or v3)", s)
	}
}

// Fallback decides what a box does when the content route it prefers is missing.
//
// The trigger is a 404 and only a 404: that is the one condition under which the
// other route is worth trying. Treating every failure as fallback-eligible hides
// real ones — a server error, an over-long chapter list and a deliberately
// aborted body are all conditions the other route shares.
type Fallback uint8

// Fallback policies.
const (
	// FallbackNone never retries on the other path.
	FallbackNone Fallback = iota
	// FallbackOn404 retries as v2 when the v3 route returns 404.
	FallbackOn404
	// FallbackAlways prefers v2 and falls back to v3, inverting the configured
	// order. It models a box whose firmware simply reaches for the older route
	// first.
	FallbackAlways
)

func (f Fallback) String() string {
	switch f {
	case FallbackOn404:
		return "on-404"
	case FallbackAlways:
		return "always"
	default:
		return "off"
	}
}

// ParseFallback maps a flag value onto a Fallback.
func ParseFallback(s string) (Fallback, error) {
	switch strings.ToLower(s) {
	case "off", "none":
		return FallbackNone, nil
	case "on-404":
		return FallbackOn404, nil
	case "always":
		return FallbackAlways, nil
	default:
		return 0, fmt.Errorf("toniebox: unknown fallback policy %q (want off, on-404 or always)", s)
	}
}

// Freshness selects which freshness-check route the box calls.
type Freshness uint8

// Freshness endpoints.
const (
	FreshnessV1 Freshness = iota
	FreshnessV3
	FreshnessSkip
)

func (f Freshness) String() string {
	switch f {
	case FreshnessV3:
		return "v3"
	case FreshnessSkip:
		return "none"
	default:
		return "v1"
	}
}

// ParseFreshness maps a flag value onto a Freshness.
func ParseFreshness(s string) (Freshness, error) {
	switch strings.ToLower(s) {
	case "v1":
		return FreshnessV1, nil
	case "v3":
		return FreshnessV3, nil
	case "none", "skip":
		return FreshnessSkip, nil
	default:
		return 0, fmt.Errorf("toniebox: unknown freshness endpoint %q (want v1, v3 or none)", s)
	}
}

// ReportAudioID decides what the box claims to have cached when it
// freshness-checks. The stored TAF audio id is what real hardware reports;
// anything else is an experiment.
type ReportAudioID uint8

// Report policies.
const (
	// ReportTAF reports the audio id parsed out of the TAF header last
	// downloaded.
	ReportTAF ReportAudioID = iota
	// ReportZero reports 0, reproducing a box with nothing cached.
	ReportZero
	// ReportStale reports an audio id one below the stored one, reproducing a
	// box whose cache is older than the server's.
	ReportStale
	// ReportOmit sends no audio id at all, which for the v1 route means no body
	// and for the v3 route means a uid with no value.
	ReportOmit
)

func (r ReportAudioID) String() string {
	switch r {
	case ReportZero:
		return "zero"
	case ReportStale:
		return "stale"
	case ReportOmit:
		return "omit"
	default:
		return "taf"
	}
}

// ParseReportAudioID maps a flag value onto a ReportAudioID.
func ParseReportAudioID(s string) (ReportAudioID, error) {
	switch strings.ToLower(s) {
	case "taf":
		return ReportTAF, nil
	case "zero":
		return ReportZero, nil
	case "stale":
		return ReportStale, nil
	case "omit":
		return ReportOmit, nil
	default:
		return 0, fmt.Errorf("toniebox: unknown audio-id report policy %q (want taf, zero, stale or omit)", s)
	}
}

// RangeOffset decides where in the content body a download starts. This is the
// lever that decides whether teddyCloud starts a fresh ffmpeg and the shim sees
// a new /stream request, or continues the existing partial TAF and the shim sees
// nothing (src/handler_cloud.c:645, src/toniefile.c:119-135).
type RangeOffset uint8

// Range policies.
const (
	// RangeFromZero always requests bytes=0-, which teddyCloud ignores as a range
	// and answers with a full 200 body.
	RangeFromZero RangeOffset = iota
	// RangeResume continues from the last byte the box received.
	RangeResume
)

func (r RangeOffset) String() string {
	if r == RangeResume {
		return "resume"
	}
	return "zero"
}

// ParseRangeOffset maps a flag value onto a RangeOffset.
func ParseRangeOffset(s string) (RangeOffset, error) {
	switch strings.ToLower(s) {
	case "zero", "0":
		return RangeFromZero, nil
	case "resume":
		return RangeResume, nil
	default:
		return 0, fmt.Errorf("toniebox: unknown range policy %q (want zero or resume)", s)
	}
}

// Config is the complete set of switchable box-behaviour assumptions. Every
// value here encodes something the mock cannot know about real firmware, which
// is why they are all switches and why the whole set is stamped into every
// assertion report: a result is only attributable alongside the assumptions that
// produced it.
type Config struct {
	Firmware Firmware
	MAC      string
	UID      uint64

	ContentPath ContentPath
	Fallback    Fallback
	Freshness   Freshness
	Report      ReportAudioID
	Recheck     bool
	Range       RangeOffset

	// Speed is the read rate as a multiple of real time. 1 is a box playing
	// normally, 0.5 a struggling speaker or a slow link, and values above 1 a
	// consumer draining faster than ffmpeg produces.
	//
	// Zero is deliberately not "stop reading": that condition is expressed by the
	// download gate, which models a consumer that paused without disconnecting,
	// and a zero rate here would only mean "do not pace". The distinction matters
	// because the two are different faults and the report has to be able to tell
	// them apart.
	Speed float64
	// StallMS pauses reading mid-body without closing the connection.
	StallMS int
	// DropAfter aborts the body after N bytes. Combined with RangeResume, the
	// reconnect then exercises teddyCloud's append path.
	DropAfter int64
	// MaxDownload stops after N bytes and goes idle, holding the connection.
	MaxDownload int64
	// DoublePressMS is the gap between the volume frame and the ear-press frame.
	// It must stay under MultiPressWindow or teddyCloud will not report a double.
	DoublePressMS int
	// PadField6 appends filler bytes to field6, to check that teddyCloud's
	// readers tolerate a longer payload than they consume.
	PadField6 int
}

// DefaultConfig returns the configuration a plain run uses: a CC3200 box that
// plays at real time, uses the v3 content path, and always starts from byte zero.
func DefaultConfig(mac string, uid uint64) Config {
	return Config{
		Firmware:      CC3200,
		MAC:           mac,
		UID:           uid,
		ContentPath:   ContentV3,
		Fallback:      FallbackOn404,
		Freshness:     FreshnessV1,
		Report:        ReportTAF,
		Recheck:       true,
		Range:         RangeFromZero,
		Speed:         1,
		DoublePressMS: int(MultiPressWindow / time.Millisecond / 2),
	}
}

// Assumptions renders the configuration as an ordered key/value list for
// stamping into a report.
func (c Config) Assumptions() [][2]string {
	return [][2]string{
		{"firmware", c.Firmware.String()},
		{"mac", c.MAC},
		{"uid", RUIDHex(c.UID)},
		{"content_path", c.ContentPath.String()},
		{"fallback", c.Fallback.String()},
		{"freshness", c.Freshness.String()},
		{"report_audio_id", c.Report.String()},
		{"recheck", fmt.Sprint(c.Recheck)},
		{"range_offset", c.Range.String()},
		{"speed", fmt.Sprintf("%g", c.Speed)},
		{"stall_ms", fmt.Sprint(c.StallMS)},
		{"drop_after_bytes", fmt.Sprint(c.DropAfter)},
		{"max_download_bytes", fmt.Sprint(c.MaxDownload)},
		{"double_press_ms", fmt.Sprint(c.DoublePressMS)},
		{"pad_field6_bytes", fmt.Sprint(c.PadField6)},
	}
}

// Box holds the state a physical box keeps between requests: when it booted,
// which tag it carries, what it believes it has cached, and how far it has read.
// A real box is stateful, and the transitions are where the interesting
// behaviour lives — a mock that replays a fixed request script would pass while
// every transition rotted.
type Box struct {
	mu  sync.Mutex
	cfg Config

	started  time.Time
	sequence uint32

	uid        uint64
	tagPresent bool

	cachedAudioID uint32
	hasCached     bool

	// bytesReceived is the running total across downloads on this connection,
	// which is what RangeResume continues from.
	bytesReceived int64
	// lastAudioID is the audio id parsed out of the most recent TAF header.
	lastAudioID uint32
	// lastVolumeUptime is the uptime of the last volume frame this box emitted.
	// teddyCloud arms its double-press window from that frame and never clears
	// it except by seeing a double (src/handler_rtnl.c:433-437), so spacing
	// volume frames out is what keeps an ordinary press ordinary.
	lastVolumeUptime uint64
}

// NewBox returns a box with the given assumptions, with its clock started now so
// that uptime begins near zero.
func NewBox(cfg Config) *Box {
	return &Box{cfg: cfg, started: time.Now(), uid: cfg.UID}
}

// Config returns the box's assumptions.
func (b *Box) Config() Config {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cfg
}

// Overlay returns the overlay id derived from the box MAC.
func (b *Box) Overlay() string { return b.cfg.MAC }

// Uptime returns milliseconds since the box started. It is strictly monotonic by
// construction, which matters because teddyCloud's double-press window is an
// unsigned subtraction against it.
func (b *Box) Uptime() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.uptimeLocked()
}

func (b *Box) uptimeLocked() uint64 {
	ms := uint64(time.Since(b.started).Milliseconds())
	if ms == 0 {
		// Keep it moving. A constant uptime makes every repeat ear press
		// register as a double, because the window check is
		// `uptime - lastEarpress < window` on unsigned values.
		ms = 1
	}
	return ms
}

// BytesReceived returns how many content bytes this box has read in total.
func (b *Box) BytesReceived() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.bytesReceived
}

// CachedAudioID returns the audio id the box would report, applying the
// configured policy.
func (b *Box) CachedAudioID() (uint32, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reportedAudioIDLocked()
}

func (b *Box) reportedAudioIDLocked() (uint32, bool) {
	switch b.cfg.Report {
	case ReportZero:
		return 0, true
	case ReportStale:
		if !b.hasCached {
			return 0, true
		}
		return b.cachedAudioID - 1, true
	case ReportOmit:
		return 0, false
	default:
		// ReportTAF: an empty cache reports 0, not nothing. See FreshnessInfos.
		return b.cachedAudioID, true
	}
}

// NoteDownloadedTAF records the audio id from a TAF header the box just read, so
// that a later freshness-check can report it back the way hardware would.
func (b *Box) NoteDownloadedTAF(audioID uint32) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastAudioID = audioID
	b.cachedAudioID = audioID
	b.hasCached = true
}

// LastTAFAudioID returns the audio id from the most recent downloaded TAF.
func (b *Box) LastTAFAudioID() (uint32, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastAudioID, b.lastAudioID != 0
}

func (b *Box) nextSeqLocked() uint32 {
	b.sequence++
	return b.sequence
}

// nextSeq allocates the next sequence number. teddyCloud never validates it; it
// is incremented so teddyCloud's logs and MQTT payloads read like a real device.
func (b *Box) nextSeq() uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextSeqLocked()
}

// TagFrames renders the frames for putting a tag on the box. A valid placement
// is reported as two log3 transitions followed by the tag frame, matching the
// order teddyCloud sees in a live capture.
func (b *Box) TagFrames(kind TagKind, uid uint64) ([][]byte, error) {
	b.mu.Lock()
	uptime := b.uptimeLocked()
	seq := b.nextSeqLocked()
	b.mu.Unlock()

	var frames [][]byte
	if kind == TagValid {
		starting, err := Log3Frame(Log3PlaybackStarting, 0)
		if err != nil {
			return nil, err
		}
		frames = append(frames, starting)
	}

	tag, err := TagFrame(b.cfg.Firmware, kind, uid, uptime, seq, b.cfg.PadField6)
	if err != nil {
		return nil, err
	}
	frames = append(frames, tag)

	b.mu.Lock()
	switch kind {
	case TagValid:
		b.uid, b.tagPresent = uid, true
	case TagRemoved:
		// A removal also clears teddyCloud's tag state, which playback=stopped
		// then wipes again; the mock mirrors the box, not teddyCloud.
		b.tagPresent = false
	}
	b.mu.Unlock()

	return frames, nil
}

// Log3Frames renders a single log3 event frame.
func (b *Box) Log3Frames(eventType uint32) ([][]byte, error) {
	frame, err := Log3Frame(eventType, 0)
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

// PlaybackStopFrames renders the frames a box emits when playback stops. The
// volume frame is included because a real box reports its level as it winds
// down, and it is the only thing that arms the double-press window for a
// following press.
func (b *Box) PlaybackStopFrames(level, db int32) ([][]byte, error) {
	volume, err := b.volumeFrame(level, db, false)
	if err != nil {
		return nil, err
	}
	stopped, err := Log3Frame(Log3PlaybackStopped, 0)
	if err != nil {
		return nil, err
	}
	return [][]byte{volume, stopped}, nil
}

// VolumeFrames renders a level report on its own.
func (b *Box) VolumeFrames(level, db int32) ([][]byte, error) {
	frame, err := b.volumeFrame(level, db, false)
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

// volumeFrame renders a group-27 report at an uptime that either keeps clear of
// the double-press window or deliberately lands inside it.
//
// spacing=false skips the guard, which is what a double press wants: its second
// volume frame has to fall within MultiPressWindow of the first or teddyCloud
// never sets wasDoubleEarpress and the second press reads as a single one.
func (b *Box) volumeFrame(level, db int32, spacing bool) ([]byte, error) {
	if spacing {
		if wait := b.volumeSpacing(); wait > 0 {
			// Released while unlocked: holding b.mu across the sleep would stall
			// Uptime() for every other reader, and the uptime has to be sampled
			// after the wait anyway for the window to be measured correctly.
			time.Sleep(wait)
		}
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	uptime := b.uptimeLocked()
	frame, err := VolumeFrame(b.cfg.Firmware, level, db, uptime, b.nextSeqLocked())
	b.lastVolumeUptime = uptime
	return frame, err
}

// volumeSpacing returns how long to wait before the next ordinary volume frame
// so that it lands outside MultiPressWindow.
func (b *Box) volumeSpacing() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.lastVolumeUptime == 0 {
		return 0
	}
	// Signed, because a wrapped uptime would otherwise look like a huge positive
	// gap and silently disable the guard.
	delta := int64(b.uptimeLocked()) - int64(b.lastVolumeUptime)
	windowMS := int64(MultiPressWindow / time.Millisecond)
	if delta < 0 || delta >= windowMS {
		return 0
	}
	return MultiPressWindow - time.Duration(delta)*time.Millisecond
}

// AudioIDFrames renders an explicit audio-id report, which is what makes a
// ContentAudioId SSE event appear.
func (b *Box) AudioIDFrames(audioID uint32) ([][]byte, error) {
	frame, err := AudioIDFrame(b.cfg.Firmware, audioID, b.Uptime(), b.nextSeq())
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

// EarPressFrames renders an ear press. The volume frame must be a separate frame
// from the press: teddyCloud dispatches log3 before log2 within one RPC
// (src/handler_rtnl.c:262` then `:332`), so a combined frame would set
// lastEarpress only after the press had already been evaluated and would never
// register as a double.
func (b *Box) EarPressFrames(ear uint32, double bool, level, db int32) ([][]byte, error) {
	// The window check runs on the volume frame, not on the press
	// (src/handler_rtnl.c:433-437), so the press cannot arm itself.
	first, err := b.volumeFrame(level, db, true)
	if err != nil {
		return nil, err
	}
	press, err := Log3Frame(ear, 0)
	if err != nil {
		return nil, err
	}
	frames := [][]byte{first, press}
	if !double {
		return frames, nil
	}

	// A double is volume, press, volume, press — two volume frames inside the
	// window. Two presses alone never qualify: wasDoubleEarpress is only ever
	// set by the group-27 handler, and nothing in the log3 path sets it.
	time.Sleep(time.Duration(b.cfg.DoublePressMS) * time.Millisecond)

	second, err := b.volumeFrame(level, db, false)
	if err != nil {
		return nil, err
	}
	again, err := Log3Frame(ear, 0)
	if err != nil {
		return nil, err
	}
	return append(frames, second, again), nil
}

// TelemetryFrames renders the informational frames a real box emits on connect:
// its firmware version, its region, and the domains it talks to. Their only
// effect is populating internal.toniebox_firmware and internal.rtnl, but they
// are what makes teddyCloud's box detail page look like a real device.
func (b *Box) TelemetryFrames() ([][]byte, error) {
	specs := []struct {
		group, function uint32
		value           string
	}{
		{GroupFirmware, FirmwareRTNLVersion, "105"},
		{GroupFirmware, FirmwareRTNLFullVersion, "2.105.00"},
		{GroupNetworkHTTP, NetworkRegion, "EU"},
		{GroupNetworkHTTP, NetworkProdDomain, "prod.teddyrock.com"},
		{GroupNetworkTCP, NetworkRTNLDomain, "rtnl.teddyrock.com"},
	}

	frames := make([][]byte, 0, len(specs)+1)
	for _, spec := range specs {
		frame, err := NetworkFrame(spec.group, spec.function, spec.value, b.Uptime(), b.nextSeq())
		if err != nil {
			return nil, err
		}
		frames = append(frames, frame)
	}

	detail, err := FirmwareDetailFrame("VLS1", b.Uptime(), b.nextSeq())
	if err != nil {
		return nil, err
	}
	return append(frames, detail), nil
}

// FreshnessInfos builds the report for the current placement.
//
// An empty cache is reported as audio id 0 rather than suppressing the exchange:
// that is what hardware does on a first placement, and skipping it would leave
// the freshness routes — and the v3 route in particular — unexercised for the
// whole first cycle of a run. ReportOmit is the only policy that withholds the
// body, because withholding it is what it is for.
func (b *Box) FreshnessInfos() ([]FCInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	uid := b.uid
	if !b.tagPresent {
		uid = b.cfg.UID
	}
	audioID, ok := b.reportedAudioIDLocked()
	if !ok {
		return nil, nil
	}
	return []FCInfo{{UID: uid, AudioID: audioID}}, nil
}

// AddBytes advances the running content byte count that RangeResume continues
// from.
func (b *Box) AddBytes(n int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytesReceived += n
}

// CurrentUID returns the uid the box currently reports as placed.
func (b *Box) CurrentUID() (uint64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.uid, b.tagPresent
}
