package toniebox

import (
	"encoding/binary"
	"fmt"
)

// TAFHeaderLength is the fixed size of the header teddyCloud writes at the front
// of every TAF (TONIE_HEADER_LENGTH, include/net_config.h:69).
const TAFHeaderLength = 0x1000

// MaxChapters is the hard chapter ceiling. toniefile_new_chapter refuses at
// n_track_page_nums >= TONIEFILE_MAX_CHAPTERS - 1 (src/toniefile.c:389-392), so a
// playlist longer than this is silently truncated.
const MaxChapters = 99

// TAFHeader is the subset of TonieboxAudioFileHeader (proto/toniebox.pb.taf-header.proto)
// that the mock reads back out of what it downloaded, so that it can report a
// cached audio id the way hardware does and assert the structure teddyCloud
// claims to have written.
type TAFHeader struct {
	SHA1           []byte
	NumBytes       uint64
	AudioID        uint32
	TrackPageNums  []uint32
	OggGranulePos  uint64
	HasGranule     bool
	OggPacketCount uint64
	HasPacketCount bool
	TAFBlockNum    uint64
	HasBlockNum    bool
	Pageno         uint64
	HasPageno      bool
}

// ParseTAFHeader decodes the header block at the front of a TAF body.
//
// The expected input is the first TAFHeaderLength bytes exactly as teddyCloud
// writes them: a four-byte big-endian length, then that many bytes of
// protobuf, then zero padding out to the block boundary
// (src/toniefile.c:311-345). A buffer with no plausible length prefix — a body
// read from an offset, for instance — is parsed as a bare protobuf instead.
//
// Like the freshness response, an unknown field is an error rather than a skip:
// this parser exists to catch a header that is not what teddyCloud says it
// writes, and silently ignoring the part it does not recognise would defeat
// that.
func ParseTAFHeader(b []byte) (TAFHeader, error) {
	var h TAFHeader

	if len(b) >= 4 {
		if protoLen := binary.BigEndian.Uint32(b); protoLen > 0 && int(protoLen) <= len(b)-4 {
			b = b[4 : 4+protoLen]
		}
	}

	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return h, fmt.Errorf("toniebox: TAF header truncated in field tag")
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&7)

		if wire == wireVarint {
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return h, fmt.Errorf("toniebox: TAF header truncated in field %d", field)
			}
			b = b[n:]
			switch field {
			case 2:
				h.NumBytes = v
			case 3:
				h.AudioID = uint32(v)
			case 6:
				h.OggGranulePos, h.HasGranule = v, true
			case 7:
				h.OggPacketCount, h.HasPacketCount = v, true
			case 8:
				h.TAFBlockNum, h.HasBlockNum = v, true
			case 9:
				h.Pageno, h.HasPageno = v, true
			default:
				return h, fmt.Errorf("toniebox: TAF header has unknown varint field %d", field)
			}
			continue
		}

		if wire == wireBytes {
			size, n := binary.Uvarint(b)
			if n <= 0 {
				return h, fmt.Errorf("toniebox: TAF header truncated in field %d length", field)
			}
			b = b[n:]
			if size > uint64(len(b)) {
				return h, fmt.Errorf("toniebox: TAF header field %d claims %d bytes, %d remain", field, size, len(b))
			}
			switch field {
			case 1:
				h.SHA1 = append([]byte(nil), b[:size]...)
			case 4:
				// packed=true, so track_page_nums arrives as a byte run of varints
				for rest := b[:size]; len(rest) > 0; {
					v, n := binary.Uvarint(rest)
					if n <= 0 {
						return h, fmt.Errorf("toniebox: TAF header track_page_nums truncated")
					}
					h.TrackPageNums = append(h.TrackPageNums, uint32(v))
					rest = rest[n:]
				}
			case 5:
				// _fill, never interesting
			default:
				return h, fmt.Errorf("toniebox: TAF header has unknown bytes field %d", field)
			}
			b = b[size:]
			continue
		}

		return h, fmt.Errorf("toniebox: TAF header field %d has unexpected wire type %d", field, wire)
	}

	return h, nil
}
