package toniebox

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildTAFBlock assembles a header block the way toniefile_write_header does: a
// four-byte big-endian protobuf length, the protobuf, then zero padding out to
// TAFHeaderLength (src/toniefile.c:311-345).
func buildTAFBlock(t *testing.T, proto []byte) []byte {
	t.Helper()

	if len(proto)+4 > TAFHeaderLength {
		t.Fatalf("protobuf is %d bytes, will not fit a %d-byte header", len(proto), TAFHeaderLength)
	}
	block := make([]byte, TAFHeaderLength)
	binary.BigEndian.PutUint32(block, uint32(len(proto)))
	copy(block[4:], proto)
	return block
}

func tafTestHeader() []byte {
	var proto []byte

	sha1 := bytes.Repeat([]byte{0xAB}, 20)
	proto = append(proto, 0x0A) // field 1, LEN
	proto = append(proto, byte(len(sha1)))
	proto = append(proto, sha1...)

	proto = append(proto, 0x10) // field 2, varint
	proto = binary.AppendUvarint(proto, 40960)

	proto = append(proto, 0x18) // field 3, varint
	proto = binary.AppendUvarint(proto, 372444745)

	proto = append(proto, 0x22, 0x03) // field 4, packed: three varints
	proto = binary.AppendUvarint(proto, 0)
	proto = binary.AppendUvarint(proto, 1)
	proto = binary.AppendUvarint(proto, 2)

	proto = append(proto, 0x2A, 0x04, 0, 0, 0, 0) // field 5, _fill

	proto = append(proto, 0x30) // field 6
	proto = binary.AppendUvarint(proto, 1000)
	proto = append(proto, 0x38) // field 7
	proto = binary.AppendUvarint(proto, 2000)
	proto = append(proto, 0x40) // field 8
	proto = binary.AppendUvarint(proto, 3)
	proto = append(proto, 0x48) // field 9, pageno
	proto = binary.AppendUvarint(proto, 7)

	return proto
}

// TestParseTAFHeaderBlock covers the shape teddyCloud actually writes. Field 9
// (pageno) is set unconditionally by toniefile_write_header
// (src/toniefile.c:326-330), so a parser that stops at field 8 rejects every
// header teddyCloud produces — which is exactly the failure the first version of
// this parser had.
func TestParseTAFHeaderBlock(t *testing.T) {
	block := buildTAFBlock(t, tafTestHeader())

	h, err := ParseTAFHeader(block)
	if err != nil {
		t.Fatalf("ParseTAFHeader: %v", err)
	}
	if h.AudioID != 372444745 {
		t.Errorf("AudioID = %d, want 372444745", h.AudioID)
	}
	if h.NumBytes != 40960 {
		t.Errorf("NumBytes = %d, want 40960", h.NumBytes)
	}
	if len(h.SHA1) != 20 || h.SHA1[0] != 0xAB {
		t.Errorf("SHA1 = % X, want 20 bytes of AB", h.SHA1)
	}
	if len(h.TrackPageNums) != 3 || h.TrackPageNums[2] != 2 {
		t.Errorf("TrackPageNums = %v, want [0 1 2]", h.TrackPageNums)
	}
	if !h.HasGranule || h.OggGranulePos != 1000 {
		t.Errorf("OggGranulePos = %d (has=%v), want 1000", h.OggGranulePos, h.HasGranule)
	}
	if !h.HasPacketCount || h.OggPacketCount != 2000 {
		t.Errorf("OggPacketCount = %d (has=%v), want 2000", h.OggPacketCount, h.HasPacketCount)
	}
	if !h.HasBlockNum || h.TAFBlockNum != 3 {
		t.Errorf("TAFBlockNum = %d (has=%v), want 3", h.TAFBlockNum, h.HasBlockNum)
	}
	if !h.HasPageno || h.Pageno != 7 {
		t.Errorf("Pageno = %d (has=%v), want 7", h.Pageno, h.HasPageno)
	}
}

// TestParseTAFHeaderBare checks the fallback for a buffer with no usable length
// prefix, which is what a body read from a Range offset looks like.
func TestParseTAFHeaderBare(t *testing.T) {
	proto := tafTestHeader()

	h, err := ParseTAFHeader(proto)
	if err != nil {
		t.Fatalf("ParseTAFHeader on a bare protobuf: %v", err)
	}
	if h.AudioID != 372444745 {
		t.Errorf("AudioID = %d, want 372444745", h.AudioID)
	}
}

// TestParseTAFHeaderTruncated makes sure a block whose declared length runs past
// the end of the buffer is rejected rather than parsed as though the prefix were
// part of the message.
func TestParseTAFHeaderTruncated(t *testing.T) {
	block := buildTAFBlock(t, tafTestHeader())
	// Claim far more protobuf than the buffer holds. The fallback then tries to
	// parse the prefix itself, which begins with field 0 and is refused.
	binary.BigEndian.PutUint32(block, uint32(TAFHeaderLength))

	if _, err := ParseTAFHeader(block); err == nil {
		t.Error("block with an impossible length prefix was accepted, want an error")
	}
	if _, err := ParseTAFHeader([]byte{0x01, 0x02}); err == nil {
		t.Error("two-byte buffer was accepted, want an error")
	}
}

// TestParseTAFHeaderRejectsUnknownField keeps the same discipline as the
// freshness parser: a header field the mock does not know means the schema moved.
func TestParseTAFHeaderRejectsUnknownField(t *testing.T) {
	var proto []byte
	proto = append(proto, 0x18, 0x2A) // audio_id = 42
	proto = append(proto, 0x50, 0x01) // field 10, varint — not in the proto

	if _, err := ParseTAFHeader(buildTAFBlock(t, proto)); err == nil {
		t.Error("unknown field 10 was accepted, want an error")
	}
}

// TestMaxChapters pins the ceiling toniefile enforces, so that a content-meta
// reply listing more chapters than the writer can produce is caught at the
// boundary rather than silently dropped later.
func TestMaxChapters(t *testing.T) {
	if MaxChapters != 99 {
		t.Errorf("MaxChapters = %d, want 99 (TONIEFILE_MAX_CHAPTERS - 1)", MaxChapters)
	}
}
