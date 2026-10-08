package toniebox

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// rawField is one field walked out of an encoded protobuf message.
type rawField struct {
	field int
	wire  int
	// val holds the value for fixed wire types and the payload for LEN.
	val []byte
	// num holds the value for varint fields.
	num uint64
}

// walk parses b into its fields. It is deliberately strict: the encoders under
// test only ever emit the wire types protobuf defines, so anything else is a bug
// in the encoder rather than something to tolerate.
func walk(t *testing.T, b []byte) []rawField {
	t.Helper()

	var out []rawField
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			t.Fatalf("wire type tag truncated")
		}
		b = b[n:]
		f, wire := int(tag>>3), int(tag&7)

		switch wire {
		case wireVarint:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				t.Fatalf("field %d varint truncated", f)
			}
			b = b[n:]
			out = append(out, rawField{field: f, wire: wire, num: v})
		case wireFixed64:
			if len(b) < 8 {
				t.Fatalf("field %d fixed64 truncated", f)
			}
			out = append(out, rawField{field: f, wire: wire, val: b[:8]})
			b = b[8:]
		case wireFixed32:
			if len(b) < 4 {
				t.Fatalf("field %d fixed32 truncated", f)
			}
			out = append(out, rawField{field: f, wire: wire, val: b[:4]})
			b = b[4:]
		case wireBytes:
			size, n := binary.Uvarint(b)
			if n <= 0 {
				t.Fatalf("field %d length truncated", f)
			}
			b = b[n:]
			if uint64(len(b)) < size {
				t.Fatalf("field %d claims %d bytes, %d remain", f, size, len(b))
			}
			out = append(out, rawField{field: f, wire: wire, val: b[:size]})
			b = b[size:]
		default:
			t.Fatalf("field %d has unknown wire type %d", f, wire)
		}
	}
	return out
}

// splitFrame unwraps the four-byte big-endian length prefix and returns the
// RPC payload, checking that the prefix agrees with what is actually there —
// which is exactly the check teddyCloud performs before it trusts the payload
// (src/handler_rtnl.c:102-115).
func splitFrame(t *testing.T, frame []byte) []byte {
	t.Helper()

	if len(frame) < 4 {
		t.Fatalf("frame is %d bytes, shorter than the length prefix", len(frame))
	}
	declared := binary.BigEndian.Uint32(frame[:4])
	if int(declared) != len(frame)-4 {
		t.Fatalf("prefix says %d bytes but %d follow", declared, len(frame)-4)
	}
	if frame[0] != 0 || frame[1] != 0 {
		t.Fatalf("prefix high bytes are % 02x, teddyCloud rejects anything non-zero", frame[:2])
	}
	return frame[4:]
}

func find(t *testing.T, fields []rawField, want int) rawField {
	t.Helper()
	for _, f := range fields {
		if f.field == want {
			return f
		}
	}
	t.Fatalf("no field %d present", want)
	return rawField{}
}

// TestFrameGoldenPlaybackStarting pins the worked frame from
// docs/mock-toniebox-design.md §6.7: log3 with datetime 0 and field2 11.
func TestFrameGoldenPlaybackStarting(t *testing.T) {
	got, err := Log3Frame(Log3PlaybackStarting, 0)
	if err != nil {
		t.Fatalf("Log3Frame: %v", err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x09,
		0x1A, 0x07,
		0x0D, 0x00, 0x00, 0x00, 0x00,
		0x10, 0x0B,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("playback starting frame:\n got % 02X\nwant % 02X", got, want)
	}
}

// TestFrameGoldenTagPlaced pins the second worked frame: a CC3200 tag placement
// for rUID fa9c3e0d500304e0 at uptime and sequence zero.
//
// The varint for function 8627 is B3 43 (8627 = 67*128 + 51) and field3 is a
// required field that is always emitted, so the log2 body is 28 bytes rather
// than the 24 an earlier draft of the design claimed.
func TestFrameGoldenTagPlaced(t *testing.T) {
	// This is the *uid*, the value read_big_endian64 produces, not the rUID that
	// appears in a content URL. The two are byte-reversed relative to each other
	// (src/handler.c:647-658), and swapping them yields a 404 for a figurine that
	// is perfectly present — see TestRUIDRoundTrip.
	const uid = uint64(0xFA9C3E0D500304E0)

	got, err := TagFrame(CC3200, TagValid, uid, 0, 0, 0)
	if err != nil {
		t.Fatalf("TagFrame: %v", err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x1E,
		0x12, 0x1C,
		0x09, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x10, 0x00,
		0x18, 0x00,
		0x20, 0x0F,
		0x28, 0xB3, 0x43,
		0x32, 0x08, 0xFA, 0x9C, 0x3E, 0x0D, 0x50, 0x03, 0x04, 0xE0,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("tag placed frame:\n got % 02X\nwant % 02X", got, want)
	}
}

// TestFrameGoldenVolume checks the twelve-byte group-27 payload, which is the
// only field6 layout in the protocol whose offsets are documented directly in
// the handler (src/handler_rtnl.c:418-439).
func TestFrameGoldenVolume(t *testing.T) {
	got, err := VolumeFrame(CC3200, 12, -3, 1000, 7)
	if err != nil {
		t.Fatalf("VolumeFrame: %v", err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x22,
		0x12, 0x20,
		0x09, 0xE8, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // uptime 1000, LE
		0x10, 0x07,
		0x18, 0x00,
		0x20, 0x1B, // group 27
		0x28, 0xE0, 0x43, // function 8672 = 67*128 + 96
		0x32, 0x0C,
		0x00, 0x00, 0x00, 0x00, // ignored
		0xFD, 0xFF, 0xFF, 0xFF, // dB  = -3, LE
		0x0C, 0x00, 0x00, 0x00, // level = 12, LE
	}
	if !bytes.Equal(got, want) {
		t.Errorf("volume frame:\n got % 02X\nwant % 02X", got, want)
	}
}

// TestFrameGoldenAudioID checks that the audio id is a little-endian uint32 at
// the start of field6, which is what makes a ContentAudioId event land on the
// right number.
func TestFrameGoldenAudioID(t *testing.T) {
	got, err := AudioIDFrame(CC3200, 42, 500, 9)
	if err != nil {
		t.Fatalf("AudioIDFrame: %v", err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x1A,
		0x12, 0x18,
		0x09, 0xF4, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x10, 0x09,
		0x18, 0x00,
		0x20, 0x14, // group 20
		0x28, 0x93, 0x35, // function 6803 = 53*128 + 19
		0x32, 0x04, 0x2A, 0x00, 0x00, 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("audio id frame:\n got % 02X\nwant % 02X", got, want)
	}
}

// TestFrameGoldenRegion is the first of the string-carrying frames. Its payload
// is "EU" plus a NUL, and teddyCloud reads it by casting field6 to `const char *`
// (src/handler_rtnl.c:466-470), so that trailing zero is part of the contract
// rather than padding.
func TestFrameGoldenRegion(t *testing.T) {
	got, err := NetworkFrame(GroupNetworkHTTP, NetworkRegion, "EU", 10, 2)
	if err != nil {
		t.Fatalf("NetworkFrame: %v", err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x19,
		0x12, 0x17,
		0x09, 0x0A, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, // uptime 10, LE
		0x10, 0x02,
		0x18, 0x00,
		0x20, 0x06, // group 6
		0x28, 0x97, 0x06, // function 791 = 6*128 + 23
		0x32, 0x03, 'E', 'U', 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("region frame:\n got % 02X\nwant % 02X", got, want)
	}
}

// TestFrameGoldenFirmwareDetail pins the eight-byte offset: teddyCloud reads this
// string from &field6.data[8], not from the start (src/handler_rtnl.c:451), so
// the payload is eight filler bytes and then the value.
func TestFrameGoldenFirmwareDetail(t *testing.T) {
	got, err := FirmwareDetailFrame("VLS1", 10, 1)
	if err != nil {
		t.Fatalf("FirmwareDetailFrame: %v", err)
	}
	want := []byte{
		0x00, 0x00, 0x00, 0x23,
		0x12, 0x21,
		0x09, 0x0A, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x10, 0x01,
		0x18, 0x00,
		0x20, 0x08, // group 8
		0x28, 0xEA, 0x37, // function 7146 = 55*128 + 106
		0x32, 0x0D, // 8 filler + "VLS1" + NUL
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		'V', 'L', 'S', '1', 0x00,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("firmware detail frame:\n got % 02X\nwant % 02X", got, want)
	}
}

// TestStringFramesAreNULTerminated is the guard for a crash that is invisible
// from the mock's side.
//
// teddyCloud reads every string-carrying log2 by casting field6 to `const char *`
// and passing it to settings_set_string_id, which ends in strdup
// (src/handler_rtnl.c:440-482, src/settings.c:1546). protobuf-c allocates a
// `bytes` field exactly its declared length and appends no terminator, so a
// payload that omits the NUL makes teddyCloud read past the end of its own
// allocation. Its AddressSanitizer build reports that as a heap-buffer-overflow
// and a release build turns it into a crash that takes the whole server down.
//
// An empty field6 is the same defect by another route, and it is what a
// destination-only slice copy produces — so this asserts both properties, on
// every string frame the mock can emit.
func TestStringFramesAreNULTerminated(t *testing.T) {
	// offset is where teddyCloud starts reading the string: zero for most of
	// these, eight for the firmware detail function
	// (src/handler_rtnl.c:451).
	type stringFrame struct {
		build  func() ([]byte, error)
		offset int
	}

	cases := map[string]stringFrame{
		"rtnlVersion":     {func() ([]byte, error) { return FirmwareFrame(FirmwareRTNLVersion, "105", 10, 1) }, 0},
		"rtnlFullVersion": {func() ([]byte, error) { return FirmwareFrame(FirmwareRTNLFullVersion, "2.105.00", 10, 2) }, 0},
		"rtnlDetail":      {func() ([]byte, error) { return FirmwareDetailFrame("VLS1", 10, 3) }, 8},
		"region":          {func() ([]byte, error) { return NetworkFrame(GroupNetworkHTTP, NetworkRegion, "EU", 10, 4) }, 0},
		"prodDomain": {func() ([]byte, error) {
			return NetworkFrame(GroupNetworkHTTP, NetworkProdDomain, "prod.teddyrock.com", 10, 5)
		}, 0},
		"rtnlDomain": {func() ([]byte, error) {
			return NetworkFrame(GroupNetworkTCP, NetworkRTNLDomain, "rtnl.teddyrock.com", 10, 6)
		}, 0},
	}

	for name, tc := range cases {
		frame, err := tc.build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		t.Run(name, func(t *testing.T) {
			log2, _ := rpcParts(t, frame)
			field6 := bytesOf(t, log2, 6)

			if len(field6) == 0 {
				t.Fatal("field6 is empty; teddyCloud would read a C string out of bounds")
			}
			if field6[len(field6)-1] != 0 {
				t.Errorf("field6 ends with %02X, not a NUL terminator", field6[len(field6)-1])
			}
			if len(field6) <= tc.offset {
				t.Fatalf("field6 is %d bytes, too short for the offset %d the handler reads from", len(field6), tc.offset)
			}

			// From the offset on, the payload must be the value and then a NUL,
			// with no interior zero — a frame carrying only filler is what a
			// slice-length mistake produces while still satisfying a length check.
			body := field6[tc.offset:]
			if !bytes.HasSuffix(body, append([]byte(nil), 0)) {
				t.Errorf("field6[%d:] = %q does not end in a NUL", tc.offset, body)
			}
			body = bytes.TrimSuffix(body, []byte{0})
			if len(body) == 0 {
				t.Errorf("field6[%d:] carries no value, only filler", tc.offset)
			}
			for i, c := range body {
				if c < 0x20 || c > 0x7E {
					t.Errorf("field6[%d][%d] = %02X is not printable ASCII", tc.offset, i, c)
				}
			}
		})
	}
}

// TestFrameRejectsEmpty covers the ErrNoLog path. teddyCloud unpacks such a
// frame, finds neither log2 nor log3, and drops it without a log line, so an
// encoder that produces one fails invisibly at runtime.
func TestFrameRejectsEmpty(t *testing.T) {
	if _, err := Frame(nil, nil); err != ErrNoLog {
		t.Errorf("Frame(nil, nil) = %v, want ErrNoLog", err)
	}
}

// TestFrameLengthCeiling pins the undocumented 64 KiB ceiling: the length prefix
// is a full uint32, but a non-zero first byte is rejected outright
// (src/handler_rtnl.c:111-115).
func TestFrameLengthCeiling(t *testing.T) {
	// The frame payload is field6's length plus 25 bytes: 17 of log2 scalar
	// tags and values, 4 for field6's own tag and three-byte length, then 4 more
	// for the RPC tag and its three-byte length. That is derived rather than
	// asserted, so adding a required field to Log2 moves the ceiling and fails
	// here instead of silently rejecting valid frames at runtime.
	const overhead = 25

	atCeiling := &Log2{FunctionGroup: GroupTag, Function: 1, Field6: make([]byte, MaxFrameLen-overhead)}
	frame, err := Frame(atCeiling, nil)
	if err != nil {
		t.Fatalf("frame exactly at the ceiling: %v", err)
	}
	if frame[0] != 0 || frame[1] != 0 {
		t.Fatalf("prefix high bytes are % 02X, want 00 00", frame[:2])
	}
	if got := len(splitFrame(t, frame)); got != MaxFrameLen {
		t.Errorf("payload is %d bytes, want %d", got, MaxFrameLen)
	}

	over := &Log2{FunctionGroup: GroupTag, Function: 1, Field6: make([]byte, MaxFrameLen-overhead+1)}
	if _, err := Frame(over, nil); err == nil {
		t.Error("frame one byte past the ceiling was accepted, want ErrFrameTooLong")
	}
}

// TestTagFramePad checks the --pad-field6 knob: filler goes after the eight uid
// bytes, so teddyCloud's read_big_endian64 still reads the same value.
func TestTagFramePad(t *testing.T) {
	uid, err := ParseRUID("E00403500EEA4BF2")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	unpadded, err := TagFrame(CC3200, TagValid, uid, 5, 1, 0)
	if err != nil {
		t.Fatalf("TagFrame: %v", err)
	}
	padded, err := TagFrame(CC3200, TagValid, uid, 5, 1, 16)
	if err != nil {
		t.Fatalf("TagFrame padded: %v", err)
	}

	a := find(t, walk(t, find(t, walk(t, splitFrame(t, unpadded)), 2).val), 6)
	b := find(t, walk(t, find(t, walk(t, splitFrame(t, padded)), 2).val), 6)

	if len(a.val) != 8 {
		t.Fatalf("unpadded field6 is %d bytes, want 8", len(a.val))
	}
	if len(b.val) != 24 {
		t.Errorf("padded field6 is %d bytes, want 24", len(b.val))
	}
	if !bytes.Equal(b.val[:8], a.val) {
		t.Errorf("padding displaced the uid: got % X, want % X", b.val[:8], a.val)
	}
}

// TestFirmwareFunctionCodes pins the per-chip function pairs. Getting these
// wrong sends a frame teddyCloud recognises as a different event, which is
// quieter than sending one it ignores.
func TestFirmwareFunctionCodes(t *testing.T) {
	cases := []struct {
		name string
		fw   Firmware
		kind TagKind
		want uint32
	}{
		{"cc3200 valid", CC3200, TagValid, 8627},
		{"cc3200 removed", CC3200, TagRemoved, 8630},
		{"cc3200 invalid", CC3200, TagInvalid, 8646},
		{"esp32 valid", ESP32, TagValid, 16065},
		{"esp32 removed", ESP32, TagRemoved, 16067},
		{"esp32 invalid", ESP32, TagInvalid, 15452},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fw.tagFunction(tc.kind); got != tc.want {
				t.Errorf("tagFunction = %d, want %d", got, tc.want)
			}
		})
	}
}
