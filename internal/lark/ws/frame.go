package ws

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// Frame is the lark WebSocket frame, encoded as protobuf on the wire. The
// field set mirrors the SDK's ws.Frame (a gogo-protobuf generated type) so a
// frame we emit is decodable by the lark server and vice-versa.
//
// Protobuf field numbers (from pbbp2.proto):
//
//	1 SeqID           uint64  varint
//	2 LogID           uint64  varint
//	3 Service         int32   varint
//	4 Method          int32   varint  (0 control, 1 data)
//	5 Headers         []Header length-delimited
//	6 PayloadEncoding string  length-delimited (optional)
//	7 PayloadType     string  length-delimited (optional)
//	8 Payload         []byte  length-delimited (optional)
//	9 LogIDNew        string  length-delimited (optional)
type Frame struct {
	SeqID           uint64
	LogID           uint64
	Service         int32
	Method          int32
	Headers         []Header
	PayloadEncoding string
	PayloadType     string
	Payload         []byte
	LogIDNew        string
}

// Header is one key/value pair of the frame headers. The wire order is
// preserved; lookups use a linear scan (Headers is small).
type Header struct {
	Key   string
	Value string
}

// Headers returns a view of the frame headers with helper accessors. The
// returned slice shares storage with the frame; do not mutate concurrently.
func (f *Frame) HeadersView() Headers { return Headers(f.Headers) }

// Headers is a []Header with linear-scan lookup helpers.
type Headers []Header

// GetString returns the first value for key, or "".
func (h Headers) GetString(key string) string {
	for _, hdr := range h {
		if hdr.Key == key {
			return hdr.Value
		}
	}
	return ""
}

// GetInt returns the first value for key parsed as a decimal int, or 0.
func (h Headers) GetInt(key string) int {
	v := h.GetString(key)
	n := 0
	for i := range v {
		c := v[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// appendHeader returns h with a new key/value pair appended. Functional form
// avoids a pointer-receiver/value-receiver mix on the Headers slice type.
func appendHeader(h Headers, key, value string) Headers {
	return append(h, Header{Key: key, Value: value})
}

// NewPingFrame builds a control-frame ping (Method=control, type=ping) seeded
// with the connection's service id. The server replies with a pong that may
// carry an updated ClientConfig in its payload.
func NewPingFrame(serviceID int32) *Frame {
	return &Frame{
		Method:  MethodControl,
		Service: serviceID,
		Headers: Headers{{Key: HeaderType, Value: TypePing}},
	}
}

// NewAckFrame clones an inbound data frame and substitutes its payload with
// the ACK body the server expects after delivering an event/card callback. The
// lark server treats an unacked data frame as a delivery failure and retries.
func NewAckFrame(in Frame, ackPayload []byte) *Frame {
	hs := make(Headers, len(in.Headers))
	copy(hs, in.Headers)
	return &Frame{
		SeqID:    in.SeqID,
		LogID:    in.LogID,
		Service:  in.Service,
		Method:   in.Method,
		Headers:  hs,
		Payload:  ackPayload,
		LogIDNew: in.LogIDNew,
	}
}

// Marshal serialises the frame as protobuf. Field order matches the SDK's
// gogo-generated encoder so the wire bytes interoperate with the lark server.
// Empty optional fields are still emitted for the scalar strings (matching
// gogo proto2 optional semantics); Payload is omitted when nil.
func (f *Frame) Marshal() ([]byte, error) {
	var b buf
	// Field 1: SeqID (varint, tag 0x08)
	b.tagVarint(1, f.SeqID)
	// Field 2: LogID (varint, tag 0x10)
	b.tagVarint(2, f.LogID)
	// Field 3: Service (varint, tag 0x18) — int32 zigzag is NOT used; lark
	// treats service as a plain varint (matches gogo varint encoding). The
	// int32→uint32→uint64 two-step is the bit-faithful conversion the SDK
	// performs; values are always non-negative in this protocol.
	b.tagVarint(3, uint64(uint32(f.Service))) //nolint:gosec // G115: bit-faithful int32→varint, matches SDK pbbp2.pb.go encoding; Service is protocol-bounded non-negative.
	// Field 4: Method (varint, tag 0x20)
	b.tagVarint(4, uint64(uint32(f.Method))) //nolint:gosec // G115: see Service above; Method is 0 (control) or 1 (data).
	// Field 5: Headers (repeated nested message, tag 0x2a)
	for _, h := range f.Headers {
		hb := marshalHeader(h)
		b.tagBytes(5, hb)
	}
	// Field 6: PayloadEncoding (string, tag 0x32)
	b.tagBytes(6, []byte(f.PayloadEncoding))
	// Field 7: PayloadType (string, tag 0x3a)
	b.tagBytes(7, []byte(f.PayloadType))
	// Field 8: Payload (bytes, tag 0x42) — emitted only when non-nil.
	if f.Payload != nil {
		b.tagBytes(8, f.Payload)
	}
	// Field 9: LogIDNew (string, tag 0x4a)
	b.tagBytes(9, []byte(f.LogIDNew))
	return b.bytes, nil
}

func marshalHeader(h Header) []byte {
	var b buf
	b.tagBytes(1, []byte(h.Key))
	b.tagBytes(2, []byte(h.Value))
	return b.bytes
}

// Unmarshal decodes a protobuf-encoded frame. Unknown fields are skipped so a
// future lark protocol addition does not break decoding. Required fields are
// not enforced: the server always sends them, and a missing one surfaces as a
// zero value which the caller handles via nil-guards.
func (f *Frame) Unmarshal(data []byte) error {
	i := 0
	for i < len(data) {
		tag, n := readVarint(data[i:])
		if n <= 0 {
			return errVarint
		}
		i += n
		field := int(tag >> 3)
		wire := int(tag & 0x7)
		switch field {
		case 1:
			if wire != 0 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			v, n := readVarint(data[i:])
			if n <= 0 {
				return errVarint
			}
			f.SeqID = v
			i += n
		case 2:
			if wire != 0 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			v, n := readVarint(data[i:])
			if n <= 0 {
				return errVarint
			}
			f.LogID = v
			i += n
		case 3:
			if wire != 0 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			v, n := readVarint(data[i:])
			if n <= 0 {
				return errVarint
			}
			// Service is an int32 wire field; reject values that would
			// overflow int32 (lark never sends these — a malformed frame
			// should not silently truncate to a bogus service id).
			if v > math.MaxInt32 {
				return fmt.Errorf("ws: service field overflow: %d", v)
			}
			f.Service = int32(v)
			i += n
		case 4:
			if wire != 0 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			v, n := readVarint(data[i:])
			if n <= 0 {
				return errVarint
			}
			if v > math.MaxInt32 {
				return fmt.Errorf("ws: method field overflow: %d", v)
			}
			f.Method = int32(v)
			i += n
		case 5:
			if wire != 2 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			msg, n := readBytes(data[i:])
			if n <= 0 {
				return errLength
			}
			i += n
			var h Header
			if err := unmarshalHeader(&h, msg); err != nil {
				return err
			}
			f.Headers = append(f.Headers, h)
		case 6:
			if wire != 2 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			s, n := readBytes(data[i:])
			if n <= 0 {
				return errLength
			}
			f.PayloadEncoding = string(s)
			i += n
		case 7:
			if wire != 2 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			s, n := readBytes(data[i:])
			if n <= 0 {
				return errLength
			}
			f.PayloadType = string(s)
			i += n
		case 8:
			if wire != 2 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			s, n := readBytes(data[i:])
			if n <= 0 {
				return errLength
			}
			f.Payload = append([]byte(nil), s...)
			i += n
		case 9:
			if wire != 2 {
				return fmt.Errorf("ws: field %d wire %d", field, wire)
			}
			s, n := readBytes(data[i:])
			if n <= 0 {
				return errLength
			}
			f.LogIDNew = string(s)
			i += n
		default:
			// Skip unknown field.
			skip, err := skipField(data[i:], wire)
			if err != nil {
				return err
			}
			i += skip
		}
	}
	return nil
}

func unmarshalHeader(h *Header, data []byte) error {
	i := 0
	for i < len(data) {
		tag, n := readVarint(data[i:])
		if n <= 0 {
			return errVarint
		}
		i += n
		field := int(tag >> 3)
		wire := int(tag & 0x7)
		switch field {
		case 1:
			s, n := readBytes(data[i:])
			if n <= 0 {
				return errLength
			}
			h.Key = string(s)
			i += n
		case 2:
			s, n := readBytes(data[i:])
			if n <= 0 {
				return errLength
			}
			h.Value = string(s)
			i += n
		default:
			skip, err := skipField(data[i:], wire)
			if err != nil {
				return err
			}
			i += skip
		}
	}
	return nil
}

// skipField advances past one unknown field of the given wire type. Returns
// the number of bytes consumed.
func skipField(data []byte, wire int) (int, error) {
	switch wire {
	case 0: // varint
		_, n := readVarint(data)
		if n <= 0 {
			return 0, errVarint
		}
		return n, nil
	case 1: // fixed64
		if len(data) < 8 {
			return 0, errLength
		}
		return 8, nil
	case 2: // length-delimited
		_, n := readBytes(data)
		if n <= 0 {
			return 0, errLength
		}
		return n, nil
	case 5: // fixed32
		if len(data) < 4 {
			return 0, errLength
		}
		return 4, nil
	default:
		return 0, fmt.Errorf("ws: unsupported wire type %d", wire)
	}
}

var (
	errVarint = errors.New("ws: malformed varint")
	errLength = errors.New("ws: malformed length-delimited field")
)

// buf is a tiny append-only byte buffer with protobuf helpers.
type buf struct{ bytes []byte }

// tagVarint writes a (field, varint) pair.
func (b *buf) tagVarint(field int, v uint64) {
	b.writeTag(field, 0)
	b.writeVarint(v)
}

// tagBytes writes a (field, length-delimited) pair.
func (b *buf) tagBytes(field int, p []byte) {
	b.writeTag(field, 2)
	b.writeVarint(uint64(len(p)))
	b.bytes = append(b.bytes, p...)
}

func (b *buf) writeTag(field, wire int) {
	b.writeVarint(uint64(field)<<3 | uint64(wire)) //nolint:gosec // G115: field is a protobuf field number (1-9), wire a wire type (0-5); both well within uint64.
}

func (b *buf) writeVarint(v uint64) {
	for v >= 0x80 {
		b.bytes = append(b.bytes, byte(v)|0x80)
		v >>= 7
	}
	b.bytes = append(b.bytes, byte(v))
}

// readVarint reads a base-128 varint. Returns (value, bytesConsumed); n<=0 on
// error.
func readVarint(data []byte) (uint64, int) {
	var v uint64
	for i := range data {
		if i >= 10 {
			return 0, -1 // varint longer than 64 bits
		}
		b := data[i]
		v |= uint64(b&0x7f) << (7 * i)
		if b < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

// readBytes reads a length-delimited field: a varint length followed by the
// payload. Returns (payload, bytesConsumed including length); n<=0 on error.
// Bounds the length to MaxInt32 before the int conversion so a malformed frame
// cannot overflow.
func readBytes(data []byte) ([]byte, int) {
	length, n := readVarint(data)
	if n <= 0 {
		return nil, -1
	}
	if length > math.MaxInt32 {
		return nil, -1
	}
	need := int(length)
	remaining := len(data) - n
	if remaining < 0 || remaining < need {
		return nil, -1
	}
	return data[n : n+need], n + need
}

// EncodeFixed64/DecodeFixed64 are unused for lark's Frame (no fixed64 fields)
// but referenced here to document the wire-type coverage and keep binary
// import meaningful for future schema additions.
var _ = binary.LittleEndian
