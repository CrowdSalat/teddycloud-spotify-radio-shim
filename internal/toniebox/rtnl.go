// Package toniebox implements the client half of the Toniebox protocol: the
// RTNL telemetry channel, the freshness-check exchange, and the box identity
// certificate teddyCloud expects from a device.
//
// It exists so cmd/mock-toniebox can drive a real teddyCloud exactly as a
// physical box would, without the box. Every wire detail here is traced to
// teddyCloud v0.7.0 (78c3b38); see docs/mock-toniebox-design.md for the full
// mapping and the design decisions. Nothing in this package is inferred from
// what a box "probably" does.
//
// The protobuf messages are small and proto2, so they are hand-encoded rather
// than generated: adding protoc and a protobuf dependency to a shim whose only
// producer is a test harness is not a trade worth making.
package toniebox

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Protobuf wire types, from the ones the RTNL schemas use.
const (
	wireVarint  = 0
	wireFixed64 = 1
	wireBytes   = 2
	wireFixed32 = 5
)

// MaxFrameLen is the largest RTNL payload teddyCloud will accept. The length
// prefix is read as a full 32-bit value, but a non-zero high byte is rejected as
// a failure — a 64 KiB ceiling the protocol does not document
// (src/handler_rtnl.c:102-115).
const MaxFrameLen = 0xFFFF

var (
	// ErrNoLog is returned for a frame carrying neither log2 nor log3.
	// teddyCloud unpacks such a frame, finds nothing to dispatch, and drops it
	// without logging anything (src/handler_rtnl.c:131-139).
	ErrNoLog = errors.New("toniebox: RTNL frame carries neither log2 nor log3")

	// ErrFrameTooLong is returned when an encoded frame would exceed MaxFrameLen.
	ErrFrameTooLong = fmt.Errorf("toniebox: RTNL frame exceeds the %d-byte ceiling teddyCloud enforces", MaxFrameLen)
)

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendTag(b []byte, field, wire int) []byte {
	return appendVarint(b, uint64(field)<<3|uint64(wire))
}

func appendUint32(b []byte, field int, v uint32) []byte {
	b = appendTag(b, field, wireVarint)
	return appendVarint(b, uint64(v))
}

func appendFixed64(b []byte, field int, v uint64) []byte {
	b = appendTag(b, field, wireFixed64)
	return binary.LittleEndian.AppendUint64(b, v)
}

func appendFixed32(b []byte, field int, v uint32) []byte {
	b = appendTag(b, field, wireFixed32)
	return binary.LittleEndian.AppendUint32(b, v)
}

func appendBytes(b []byte, field int, v []byte) []byte {
	b = appendTag(b, field, wireBytes)
	b = appendVarint(b, uint64(len(v)))
	return append(b, v...)
}

// Log2 is TonieRtnlLog2 (proto/toniebox.pb.rtnl.proto:9-19). Every field is
// required in proto2, so all five scalars are emitted even when zero: a missing
// required field makes protobuf-c's unpack return nil and teddyCloud discards
// the frame in silence (src/proto/protobuf-c.c:3258-3268), which presents as
// "teddyCloud ignored me" with nothing in the log.
type Log2 struct {
	// Uptime is milliseconds since the box booted. It must be strictly
	// monotonic: the ear-press double window is an unsigned subtraction against
	// it, so a constant value makes every repeat press a double and a value
	// that goes backwards wraps and never doubles
	// (src/handler_rtnl.c:433-437).
	Uptime uint64
	// Sequence is never validated by teddyCloud; sent for log readability.
	Sequence uint32
	// Field3 is never validated; sent for log readability.
	Field3        uint32
	FunctionGroup uint32
	Function      uint32
	// Field6 is the payload, interpreted per (group, function). teddyCloud does
	// no length checking on it in any branch (src/handler_rtnl.c:336-492), so a
	// short payload is an out-of-bounds read rather than a rejection.
	Field6 []byte
}

func (l *Log2) marshal() []byte {
	b := make([]byte, 0, 40+len(l.Field6))
	b = appendFixed64(b, 1, l.Uptime)
	b = appendUint32(b, 2, l.Sequence)
	b = appendUint32(b, 3, l.Field3)
	b = appendUint32(b, 4, l.FunctionGroup)
	b = appendUint32(b, 5, l.Function)
	return appendBytes(b, 6, l.Field6)
}

// Log3 is TonieRtnlLog3 (proto/toniebox.pb.rtnl.proto:21-24). The event type is
// field 2.
type Log3 struct {
	// Datetime is never validated.
	Datetime uint32
	Type     uint32
}

func (l *Log3) marshal() []byte {
	b := make([]byte, 0, 12)
	b = appendFixed32(b, 1, l.Datetime)
	return appendUint32(b, 2, l.Type)
}

// Frame renders a complete RTNL frame: a four-byte big-endian payload length
// followed by a TonieRtnlRPC (src/handler_rtnl.c:85-148).
//
// teddyCloud dispatches log3 before log2 inside a single RPC
// (src/handler_rtnl.c:262` then `:332`), so a frame carrying both cannot use
// log2 to arm the double-press window for a log3 ear press in that same frame.
// Callers that need the window armed must send two frames.
func Frame(log2 *Log2, log3 *Log3) ([]byte, error) {
	if log2 == nil && log3 == nil {
		return nil, ErrNoLog
	}

	var payload []byte
	if log2 != nil {
		payload = appendBytes(payload, 2, log2.marshal())
	}
	if log3 != nil {
		payload = appendBytes(payload, 3, log3.marshal())
	}

	if len(payload) > MaxFrameLen {
		return nil, ErrFrameTooLong
	}

	frame := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	return append(frame, payload...), nil
}
