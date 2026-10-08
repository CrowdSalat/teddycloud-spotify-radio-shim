package toniebox

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"time"
)

// log2 function groups (include/handler_rtnl.h:16-27).
const (
	GroupNetworkHTTP uint32 = 6
	GroupFirmware    uint32 = 8
	GroupTilt        uint32 = 12
	GroupTag         uint32 = 15
	GroupAudioA      uint32 = 20
	GroupAudioB      uint32 = 22
	GroupVolume      uint32 = 27
	GroupNetworkTCP  uint32 = 37
)

// Tag functions. teddyCloud carries a CC3200 and an ESP32 code for every logical
// tag event, and which one a real box sends depends on its hardware
// (src/handler_rtnl.c:336-350). The mock cannot know which is right, so Firmware
// selects and the choice is recorded in every report.
const (
	TagValidCC3200   uint32 = 8627
	TagValidESP32    uint32 = 16065
	TagRemovedCC3200 uint32 = 8630
	TagRemovedESP32  uint32 = 16067
	TagInvalidCC3200 uint32 = 8646
	TagInvalidESP32  uint32 = 15452
)

// Audio-id report functions (src/handler_rtnl.c:351-400). Group 22 function 297
// is accepted for any chip; the group-20 pair is what differs.
const (
	AudioIDGroupB        uint32 = 297
	AudioIDAGroupACC3200 uint32 = 6803
	AudioIDAGroupAESP32  uint32 = 16096
)

// Volume functions. A group-27 frame also arms the ear-press double window
// (src/handler_rtnl.c:418-439).
const (
	VolumeCC3200 uint32 = 8672
	VolumeESP32  uint32 = 15524
)

// Tilt angle functions (src/handler_rtnl.c:401-417).
const (
	TiltACC3200 uint32 = 15426
	TiltBCC3200 uint32 = 15427
)

// Firmware identification functions (src/handler_rtnl.c:440-458). Detail is read
// from offset 8, not from the start of field6.
const (
	FirmwareRTNLVersion     uint32 = 704
	FirmwareRTNLFullVersion uint32 = 703
	FirmwareRTNLDetail      uint32 = 7146
)

// Region and domain functions (src/handler_rtnl.c:459-482).
const (
	NetworkRegion     uint32 = 791
	NetworkProdDomain uint32 = 1009
	NetworkRTNLDomain uint32 = 1009
)

// Log3 event types (src/handler_rtnl.c:262-330).
const (
	Log3EarBig        uint32 = 1
	Log3EarSmall      uint32 = 2
	Log3KnockForward  uint32 = 3
	Log3KnockBackward uint32 = 4
	Log3TiltForward   uint32 = 5
	Log3TiltBackward  uint32 = 6
	Log3ChargerOn     uint32 = 7
	Log3ChargerOff    uint32 = 8
	// Log3PlaybackStarting also emits playback=started: teddyCloud's handler
	// falls through into the started case because the break is missing
	// (src/toniebox_state.c:73-79). A box cannot emit one without the other.
	Log3PlaybackStarting uint32 = 11
	Log3PlaybackStarted  uint32 = 12
	Log3PlaybackStopped  uint32 = 13
)

// MultiPressWindow is teddyCloud's default ear-press double-press window,
// stored per overlay as internal.rtnl (src/settings.c:202). A group-27 volume
// frame followed by an ear press within this window of its uptime is reported
// as ear-big-double / ear-small-double.
const MultiPressWindow = 300 * time.Millisecond

// Firmware selects which of teddyCloud's paired log2 function codes the mock
// emits.
type Firmware uint8

// Firmware profiles.
const (
	CC3200 Firmware = iota
	ESP32
)

func (f Firmware) String() string {
	if f == ESP32 {
		return "esp32"
	}
	return "cc3200"
}

// ParseFirmware maps a flag value onto a Firmware.
func ParseFirmware(s string) (Firmware, error) {
	switch strings.ToLower(s) {
	case "cc3200":
		return CC3200, nil
	case "esp32":
		return ESP32, nil
	default:
		return 0, fmt.Errorf("toniebox: unknown firmware %q (want cc3200 or esp32)", s)
	}
}

func (f Firmware) tagFunction(kind TagKind) uint32 {
	if f == ESP32 {
		switch kind {
		case TagValid:
			return TagValidESP32
		case TagRemoved:
			return TagRemovedESP32
		default:
			return TagInvalidESP32
		}
	}
	switch kind {
	case TagValid:
		return TagValidCC3200
	case TagRemoved:
		return TagRemovedCC3200
	default:
		return TagInvalidCC3200
	}
}

func (f Firmware) volumeFunction() uint32 {
	if f == ESP32 {
		return VolumeESP32
	}
	return VolumeCC3200
}

func (f Firmware) audioIDFunction() uint32 {
	if f == ESP32 {
		return AudioIDAGroupAESP32
	}
	return AudioIDAGroupACC3200
}

// TagKind is the three-way outcome of putting a tag on the box.
type TagKind uint8

// Tag kinds.
const (
	TagValid TagKind = iota
	TagRemoved
	TagInvalid
)

func (k TagKind) String() string {
	switch k {
	case TagRemoved:
		return "removed"
	case TagInvalid:
		return "invalid"
	default:
		return "valid"
	}
}

// UIDBytes returns the eight field6 bytes that teddyCloud's read_big_endian64
// composes into a uid (src/handler_rtnl.c:160-169).
func UIDBytes(uid uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uid)
	return b
}

// RUIDHex returns the sixteen-character rUID teddyCloud uses in content URLs and
// content folder names. It is the byte-reversal of UIDBytes:
// getContentPathFromUID formats a uid as %016PRIX64 and then reverses the eight
// byte pairs (src/handler.c:647-658), and the content handler undoes it with
// bswap_64 (src/handler_cloud.c:489-490).
//
// An NFC uid read by the box therefore reaches teddyCloud reversed relative to
// the value printed on a figurine, which is why the rUID looks the way it does
// and why getting this backwards yields a 404 for a perfectly valid-looking id.
func RUIDHex(uid uint64) string {
	return strings.ToUpper(fmt.Sprintf("%016X", bits.ReverseBytes64(uid)))
}

// ParseRUID is the inverse of RUIDHex.
func ParseRUID(s string) (uint64, error) {
	if len(s) != 16 {
		return 0, fmt.Errorf("toniebox: rUID %q must be exactly 16 hex characters", s)
	}
	n, err := strconv.ParseUint(s, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("toniebox: rUID %q: %w", s, err)
	}
	return bits.ReverseBytes64(n), nil
}

// TagFrame renders the placement event for uid. Removal is reported over RTNL
// only: teddyCloud emits no SSE for it, so the raw log2 frame is the only
// lossless record that the tag left.
//
// pad appends filler after the eight uid bytes. teddyCloud reads field6 with
// read_big_endian64 and ignores the remainder (src/handler_rtnl.c:160-169), so a
// padded frame must still parse; the flag exists to prove that rather than to
// assume it.
func TagFrame(fw Firmware, kind TagKind, uid uint64, uptime uint64, seq uint32, pad int) ([]byte, error) {
	field6 := UIDBytes(uid)
	if pad > 0 {
		field6 = append(field6, make([]byte, pad)...)
	}
	return Frame(&Log2{
		Uptime:        uptime,
		Sequence:      seq,
		FunctionGroup: GroupTag,
		Function:      fw.tagFunction(kind),
		Field6:        field6,
	}, nil)
}

// AudioIDFrame reports the audio id the box believes it has cached. teddyCloud
// compares it against its own TAF header, and the comparison runs the opposite
// way to intuition: `updated` is true when the box's id is *older* than the
// server's (src/handler_cloud.c:963-977).
func AudioIDFrame(fw Firmware, audioID uint32, uptime uint64, seq uint32) ([]byte, error) {
	field6 := make([]byte, 4)
	binary.LittleEndian.PutUint32(field6, audioID)
	return Frame(&Log2{
		Uptime:        uptime,
		Sequence:      seq,
		FunctionGroup: GroupAudioA,
		Function:      fw.audioIDFunction(),
		Field6:        field6,
	}, nil)
}

// AudioIDGroupBFrame reports the audio id over group 22 function 297, which
// teddyCloud accepts for any chip. Useful for testing the alternative encoding
// when one of the group-20 codes turns out not to match a real box.
func AudioIDGroupBFrame(audioID uint32, uptime uint64, seq uint32) ([]byte, error) {
	field6 := make([]byte, 4)
	binary.LittleEndian.PutUint32(field6, audioID)
	return Frame(&Log2{
		Uptime:        uptime,
		Sequence:      seq,
		FunctionGroup: GroupAudioB,
		Function:      AudioIDGroupB,
		Field6:        field6,
	}, nil)
}

// VolumeFrame reports the box's speaker level and, as a side effect, arms
// teddyCloud's ear-press double window. field6 is twelve bytes: the first four
// are ignored, then a little-endian dB and a little-endian level
// (src/handler_rtnl.c:418-439).
func VolumeFrame(fw Firmware, level, db int32, uptime uint64, seq uint32) ([]byte, error) {
	field6 := make([]byte, 12)
	binary.LittleEndian.PutUint32(field6[4:], uint32(db))
	binary.LittleEndian.PutUint32(field6[8:], uint32(level))
	return Frame(&Log2{
		Uptime:        uptime,
		Sequence:      seq,
		FunctionGroup: GroupVolume,
		Function:      fw.volumeFunction(),
		Field6:        field6,
	}, nil)
}

// Log3Frame renders an event-type frame: a playback transition, an ear press, a
// knock, a tilt, or a charger change.
func Log3Frame(eventType uint32, datetime uint32) ([]byte, error) {
	return Frame(nil, &Log3{Datetime: datetime, Type: eventType})
}

// FirmwareFrame reports a firmware string. function is one of the
// Firmware* constants; the detail function's string is read from offset 8.
func FirmwareFrame(function uint32, value string, uptime uint64, seq uint32) ([]byte, error) {
	return stringLog2Frame(GroupFirmware, function, value, 0, uptime, seq)
}

// NetworkFrame reports a region or RTNL/production domain string. group is
// GroupNetworkHTTP or GroupNetworkTCP.
func NetworkFrame(group uint32, function uint32, value string, uptime uint64, seq uint32) ([]byte, error) {
	return stringLog2Frame(group, function, value, 0, uptime, seq)
}

// stringLog2Frame renders the log2 functions whose payload teddyCloud reads as a
// C string: the firmware and region/domain reports, whose detail string starts at
// offset 8 rather than at the beginning of field6
// (src/handler_rtnl.c:440-482).
//
// The NUL terminator has to be *inside* field6. teddyCloud casts the payload
// straight to `const char *` and hands it to settings_set_string_id, which ends
// in strdup, while protobuf-c allocates a `bytes` field exactly its declared
// length and adds no terminator of its own. A payload carrying only the value
// therefore makes teddyCloud walk off the end of its allocation — which its
// AddressSanitizer build reports as a heap-buffer-overflow and a release build
// would turn into a crash that takes the whole server down, not just this box.
// make zero-initialises, so the trailing byte is already the terminator.
func stringLog2Frame(group, function uint32, value string, offset int, uptime uint64, seq uint32) ([]byte, error) {
	field6 := make([]byte, offset+len(value)+1)
	copy(field6[offset:], value)
	return Frame(&Log2{
		Uptime:        uptime,
		Sequence:      seq,
		FunctionGroup: group,
		Function:      function,
		Field6:        field6,
	}, nil)
}

// FirmwareDetailFrame reports internal.toniebox_firmware.rtnl_detail, whose
// string is read from offset 8 rather than from the start of field6
// (src/handler_rtnl.c:451).
func FirmwareDetailFrame(value string, uptime uint64, seq uint32) ([]byte, error) {
	return stringLog2Frame(GroupFirmware, FirmwareRTNLDetail, value, 8, uptime, seq)
}
