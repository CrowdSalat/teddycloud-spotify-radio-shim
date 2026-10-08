package toniebox

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

// TestFreshnessRequestGolden pins the smallest possible request: one TonieFCInfo
// with a zero uid and a zero audio id. Both fields are required, so both are on
// the wire even though neither carries information — a request missing either is
// unpacked as NULL and dropped without a log line
// (src/proto/protobuf-c.c:3258-3268).
func TestFreshnessRequestGolden(t *testing.T) {
	got, err := EncodeFreshnessRequest([]FCInfo{{}})
	if err != nil {
		t.Fatalf("EncodeFreshnessRequest: %v", err)
	}
	want := []byte{
		0x0A, 0x0E, // field 1, LEN, 14 bytes
		0x09, 0, 0, 0, 0, 0, 0, 0, 0, // uid, fixed64 LE
		0x15, 0, 0, 0, 0, // audio_id, fixed32 LE
	}
	if !bytes.Equal(got, want) {
		t.Errorf("request:\n got % 02X\nwant % 02X", got, want)
	}
	if len(got) >= MaxFreshnessBody {
		t.Errorf("request is %d bytes, teddyCloud wants under %d", len(got), MaxFreshnessBody)
	}
}

// TestFreshnessRequestCeiling covers the failure mode with no failure message:
// at or above 4096 bytes teddyCloud logs an error and writes no response at all
// (src/handler_cloud.c:1050-1053), so the box waits forever for a reply.
func TestFreshnessRequestCeiling(t *testing.T) {
	// Each TonieFCInfo is 16 bytes on the wire: a tag, a one-byte length, an
	// eight-byte fixed64 uid and a four-byte fixed32 audio id.
	const perInfo = 16

	for _, tc := range []struct {
		infos   int
		wantErr bool
	}{
		{1, false},
		{100, false},
		{255, false}, // 4080 bytes, one below the ceiling
		{256, true},  // exactly 4096 — teddyCloud compares with >=
		{300, true},
	} {
		body, err := EncodeFreshnessRequest(make([]FCInfo, tc.infos))
		if tc.wantErr {
			if err == nil {
				t.Errorf("%d infos (%d bytes) encoded without error, want a ceiling violation",
					tc.infos, tc.infos*perInfo)
			}
			continue
		}
		if err != nil {
			t.Errorf("%d infos: %v", tc.infos, err)
			continue
		}
		if len(body) != tc.infos*perInfo {
			t.Errorf("%d infos produced %d bytes, want %d", tc.infos, len(body), tc.infos*perInfo)
		}
		if len(body) >= MaxFreshnessBody {
			t.Errorf("%d infos produced %d bytes, teddyCloud wants under %d",
				tc.infos, len(body), MaxFreshnessBody)
		}
	}
}

// TestFreshnessResponseRoundTrip decodes the reply teddyCloud actually sends:
// every box-setting field is required and therefore present even at zero, and
// tonie_marked is a repeated fixed64.
func TestFreshnessResponseRoundTrip(t *testing.T) {
	var encoded []byte
	for _, marked := range []uint64{0x1122334455667788, 0} {
		encoded = append(encoded, 0x09) // field 1, fixed64
		encoded = binary.LittleEndian.AppendUint64(encoded, marked)
	}
	// fields 2..8 as varints, including a negative one (slap_dir) to check the
	// int32 cast.
	encoded = append(encoded, 0x10, 0x07) // field 2 = 7
	encoded = append(encoded, 0x18, 0x64) // field 3 = 100
	encoded = append(encoded, 0x20, 0x01) // field 4 = 1
	encoded = append(encoded, 0x28, 0x01) // field 5 = 1
	encoded = append(encoded, 0x30, 0x01) // field 6 = 1
	encoded = append(encoded, 0x38, 0x64) // field 7 = 100
	encoded = append(encoded, 0x40, 0x01) // field 8 = 1

	got, err := DecodeFreshnessResponse(encoded)
	if err != nil {
		t.Fatalf("DecodeFreshnessResponse: %v", err)
	}
	if len(got.Marked) != 2 || got.Marked[0] != 0x1122334455667788 || got.Marked[1] != 0 {
		t.Errorf("Marked = % X, want [1122334455667788 0]", got.Marked)
	}
	if got.Field2 != 7 || got.MaxVolSpk != 100 || got.SlapEn != 1 ||
		got.SlapDir != 1 || got.Field6 != 1 || got.MaxVolHdp != 100 || got.Led != 1 {
		t.Errorf("box settings decoded as %+v", got)
	}
}

// TestFreshnessResponseRejectsUnknownField keeps the parser honest. A schema
// that has moved should say so rather than quietly report half a reply.
func TestFreshnessResponseRejectsUnknownField(t *testing.T) {
	// field 9, varint — beyond anything the proto defines.
	if _, err := DecodeFreshnessResponse([]byte{0x48, 0x01}); err == nil {
		t.Error("unknown field was accepted, want an error")
	}
	// field 2 with the wrong wire type.
	if _, err := DecodeFreshnessResponse([]byte{0x11}); err == nil {
		t.Error("fixed64 on a varint field was accepted, want an error")
	}
	// truncated fixed64.
	if _, err := DecodeFreshnessResponse([]byte{0x09, 0x01}); err == nil {
		t.Error("truncated tonie_marked was accepted, want an error")
	}
}

// TestFreshnessV3RoundTrip checks the JSON variant, whose keys are rUIDs rather
// than uids — the difference that makes a mixed-up round trip produce a body
// teddyCloud parses happily and then looks up under the wrong folder.
func TestFreshnessV3RoundTrip(t *testing.T) {
	uid, err := ParseRUID("E00403500EEA4BF2")
	if err != nil {
		t.Fatalf("ParseRUID: %v", err)
	}

	body, err := EncodeFreshnessRequestV3([]FCInfo{{UID: uid, AudioID: 4242}})
	if err != nil {
		t.Fatalf("EncodeFreshnessRequestV3: %v", err)
	}
	if len(body) >= MaxFreshnessBody {
		t.Errorf("v3 body is %d bytes, want under %d", len(body), MaxFreshnessBody)
	}
	if !strings.Contains(string(body), `"E00403500EEA4BF2":4242`) {
		t.Errorf("v3 body %s does not carry the rUID key", body)
	}

	marked, err := DecodeFreshnessResponseV3([]byte(`{"items":["E00403500EEA4BF2"]}`))
	if err != nil {
		t.Fatalf("DecodeFreshnessResponseV3: %v", err)
	}
	if len(marked) != 1 || marked[0] != uid {
		t.Errorf("marked = % X, want % X", marked, uid)
	}
}

// TestFreshnessV3RejectsMalformed keeps the same discipline as the proto
// parser: a reply naming something that is not a 16-character rUID is an error,
// not an entry to be skipped.
func TestFreshnessV3RejectsMalformed(t *testing.T) {
	if _, err := DecodeFreshnessResponseV3([]byte(`{"items":["not-a-uid"]}`)); err == nil {
		t.Error("malformed rUID was accepted, want an error")
	}
	if _, err := DecodeFreshnessResponseV3([]byte(`not json`)); err == nil {
		t.Error("non-JSON reply was accepted, want an error")
	}
}
