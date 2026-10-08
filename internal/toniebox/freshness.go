package toniebox

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
)

// MaxFreshnessBody is the ceiling teddyCloud enforces on a freshness-check body.
// At or above it, teddyCloud logs an error and returns without writing a
// response at all, so the box waits forever for a reply that will not come
// (src/handler_cloud.c:1050-1053). The v3 handler is blunter still: an oversize
// body tears the connection down (src/handler_cloud.c:1126-1130).
const MaxFreshnessBody = 4096

// FCInfo is one TonieFCInfo entry: a uid the box is reporting on, and the audio
// id it holds for it.
type FCInfo struct {
	UID     uint64
	AudioID uint32
}

// FreshnessResponse is TonieFreshnessCheckResponse. Only Marked matters to the
// mock; the remaining fields are box settings teddyCloud fills in from its own
// configuration (src/handler.c:660-672) and they are parsed so that a report can
// show what the box would have been told.
type FreshnessResponse struct {
	Marked    []uint64
	Field2    int32
	MaxVolSpk int32
	SlapEn    int32
	SlapDir   int32
	Field6    int32
	MaxVolHdp int32
	Led       int32
}

// EncodeFreshnessRequest renders TonieFreshnessCheckRequest.
func EncodeFreshnessRequest(infos []FCInfo) ([]byte, error) {
	var b []byte
	for _, info := range infos {
		var inner []byte
		inner = appendFixed64(inner, 1, info.UID)
		inner = appendFixed32(inner, 2, info.AudioID)
		b = appendBytes(b, 1, inner)
	}
	if len(b) >= MaxFreshnessBody {
		return nil, fmt.Errorf("toniebox: freshness request is %d bytes; teddyCloud drops anything at or above %d without replying", len(b), MaxFreshnessBody)
	}
	return b, nil
}

// DecodeFreshnessResponse parses TonieFreshnessCheckResponse.
//
// An unknown field is an error rather than a skip. The mock is a diagnostic
// instrument: a server whose schema has moved should say so loudly instead of
// quietly reporting incomplete data.
func DecodeFreshnessResponse(b []byte) (FreshnessResponse, error) {
	var resp FreshnessResponse

	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return resp, fmt.Errorf("toniebox: freshness response truncated in field tag")
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&7)

		if field == 1 {
			if wire != wireFixed64 {
				return resp, fmt.Errorf("toniebox: freshness response tonie_marked has wire type %d, want %d", wire, wireFixed64)
			}
			if len(b) < 8 {
				return resp, fmt.Errorf("toniebox: freshness response truncated in tonie_marked")
			}
			resp.Marked = append(resp.Marked, binary.LittleEndian.Uint64(b))
			b = b[8:]
			continue
		}

		v, n := binary.Uvarint(b)
		if n <= 0 {
			return resp, fmt.Errorf("toniebox: freshness response truncated in field %d", field)
		}
		b = b[n:]

		switch field {
		case 2:
			resp.Field2 = int32(v)
		case 3:
			resp.MaxVolSpk = int32(v)
		case 4:
			resp.SlapEn = int32(v)
		case 5:
			resp.SlapDir = int32(v)
		case 6:
			resp.Field6 = int32(v)
		case 7:
			resp.MaxVolHdp = int32(v)
		case 8:
			resp.Led = int32(v)
		default:
			return resp, fmt.Errorf("toniebox: freshness response has unknown field %d (wire type %d)", field, wire)
		}
	}

	return resp, nil
}

// EncodeFreshnessRequestV3 renders the JSON body of /v3/freshness-check, whose
// keys are rUIDs rather than uids (src/handler_cloud.c:1153-1191).
func EncodeFreshnessRequestV3(infos []FCInfo) ([]byte, error) {
	content := make(map[string]uint32, len(infos))
	for _, info := range infos {
		content[RUIDHex(info.UID)] = info.AudioID
	}

	body, err := json.Marshal(struct {
		Content map[string]uint32 `json:"content"`
	}{Content: content})
	if err != nil {
		return nil, fmt.Errorf("toniebox: encoding v3 freshness request: %w", err)
	}
	if len(body) >= MaxFreshnessBody {
		return nil, fmt.Errorf("toniebox: v3 freshness request is %d bytes; teddyCloud drops the connection at or above %d", len(body), MaxFreshnessBody)
	}
	return body, nil
}

// DecodeFreshnessResponseV3 parses the {"items":["<rUID>", ...]} reply and
// returns the uids it names.
func DecodeFreshnessResponseV3(b []byte) ([]uint64, error) {
	var reply struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(b, &reply); err != nil {
		return nil, fmt.Errorf("toniebox: decoding v3 freshness response: %w", err)
	}

	out := make([]uint64, 0, len(reply.Items))
	for _, item := range reply.Items {
		uid, err := ParseRUID(item)
		if err != nil {
			return nil, fmt.Errorf("toniebox: v3 freshness response: %w", err)
		}
		out = append(out, uid)
	}
	return out, nil
}
