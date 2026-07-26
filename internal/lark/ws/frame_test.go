package ws

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"testing"
)

// goldenFrames is a set of representative frames spanning the field
// combinations lark actually emits: control ping, data event (small payload),
// data card (larger payload + headers), and frames with every optional field
// populated. The table drives the round-trip and fixture-decode tests below.
func goldenFrames() []Frame {
	return []Frame{
		{
			// Minimal ping: control method, type=ping header, empty payload.
			Method:  MethodControl,
			Service: 33554678,
			Headers: Headers{{Key: HeaderType, Value: TypePing}},
		},
		{
			// Single unsplit event: data method, event type, JSON payload.
			SeqID:  100,
			LogID:  200,
			Method: MethodData,
			Headers: Headers{
				{Key: HeaderType, Value: TypeEvent},
				{Key: HeaderMessageID, Value: "om_event_1"},
				{Key: HeaderSum, Value: "1"},
				{Key: HeaderSeq, Value: "0"},
			},
			PayloadType: "application/json",
			Payload:     []byte(`{"header":{"event_id":"e1","event_type":"im.message.receive_v1"}}`),
		},
		{
			// Chunk 0 of a 2-chunk message: sum=2 seq=0.
			SeqID:  300,
			LogID:  400,
			Method: MethodData,
			Headers: Headers{
				{Key: HeaderType, Value: TypeEvent},
				{Key: HeaderMessageID, Value: "om_chunk"},
				{Key: HeaderSum, Value: "2"},
				{Key: HeaderSeq, Value: "0"},
			},
			Payload:  []byte("first chunk bytes"),
			LogIDNew: "log-new-abc",
		},
		{
			// ACK response frame (carries response payload, all fields set).
			SeqID:           500,
			LogID:           600,
			Service:         999,
			Method:          MethodData,
			PayloadEncoding: "raw",
			PayloadType:     "application/json",
			Payload:         []byte(`{"code":200}`),
			LogIDNew:        "log-new-xyz",
		},
		{
			// Empty payload + no headers.
			SeqID:   7,
			LogID:   8,
			Service: 1,
			Method:  MethodControl,
		},
	}
}

// sdkFixturesBase64 carries bytes produced by the SDK's gogo-generated
// Marshal (captured once via gen_fixtures.go against the v3.9.9 SDK). Each
// entry aligns by index with goldenFrames()/fixtureNames(). Inlining the
// fixtures as base64 keeps the test self-contained and avoids checking binary
// blobs into the source tree.
var sdkFixturesBase64 = map[string]string{
	"ping":          "CAAQABj2gYAQIAAqDAoEdHlwZRIEcGluZzIAOgBKAA==",
	"event_unsplit": "CGQQyAEYACABKg0KBHR5cGUSBWV2ZW50KhgKCm1lc3NhZ2VfaWQSCm9tX2V2ZW50XzEqCAoDc3VtEgExKggKA3NlcRIBMDIAOhBhcHBsaWNhdGlvbi9qc29uQkF7ImhlYWRlciI6eyJldmVudF9pZCI6ImUxIiwiZXZlbnRfdHlwZSI6ImltLm1lc3NhZ2UucmVjZWl2ZV92MSJ9fUoA",
	"chunk0":        "CKwCEJADGAAgASoNCgR0eXBlEgVldmVudCoWCgptZXNzYWdlX2lkEghvbV9jaHVuayoICgNzdW0SATIqCAoDc2VxEgEwMgA6AEIRZmlyc3QgY2h1bmsgYnl0ZXNKC2xvZy1uZXctYWJj",
	"ack":           "CPQDENgEGOcHIAEyA3JhdzoQYXBwbGljYXRpb24vanNvbkIMeyJjb2RlIjoyMDB9Sgtsb2ctbmV3LXh5eg==",
	"empty_control": "CAcQCBgBIAAyADoASgA=",
}

// TestFrame_MarshalUnmarshalRoundTrip confirms our own codec round-trips
// every golden frame without field loss.
func TestFrame_MarshalUnmarshalRoundTrip(t *testing.T) {
	for i, f := range goldenFrames() {
		data, err := f.Marshal()
		if err != nil {
			t.Fatalf("frame %d Marshal: %v", i, err)
		}
		var got Frame
		if err := got.Unmarshal(data); err != nil {
			t.Fatalf("frame %d Unmarshal: %v", i, err)
		}
		if !framesEqual(f, got) {
			t.Fatalf("frame %d round-trip mismatch:\nwant=%+v\ngot =%+v", i, f, got)
		}
	}
}

// TestFrame_DecodesSDKFixtures is the golden safety net (§6.3): bytes
// produced by the SDK's gogo-generated Marshal (captured once and inlined as
// base64 in sdkFixturesBase64) must decode cleanly into our Frame with every
// field intact. If this passes, the lark server (which uses the same gogo
// wire format) can talk to our decoder.
func TestFrame_DecodesSDKFixtures(t *testing.T) {
	frames := goldenFrames()
	names := []string{"ping", "event_unsplit", "chunk0", "ack", "empty_control"}
	for i, name := range names {
		raw, err := base64.StdEncoding.DecodeString(sdkFixturesBase64[name])
		if err != nil {
			t.Fatalf("fixture %s base64 decode: %v", name, err)
		}
		var got Frame
		if err := got.Unmarshal(raw); err != nil {
			t.Fatalf("fixture %s decode: %v", name, err)
		}
		if !framesEqual(frames[i], got) {
			t.Fatalf("fixture %s field mismatch:\nwant=%+v\ngot =%+v", name, frames[i], got)
		}
	}
}

// TestHeaders_Accessors pins the linear-scan lookup helpers used by the
// dispatcher and ACK builder.
func TestHeaders_Accessors(t *testing.T) {
	hs := Headers{
		{Key: HeaderType, Value: TypeEvent},
		{Key: HeaderSum, Value: "3"},
		{Key: HeaderSeq, Value: "1"},
		{Key: HeaderMessageID, Value: "om_abc"},
	}
	if got := hs.GetString(HeaderMessageID); got != "om_abc" {
		t.Fatalf("GetString message_id=%q want om_abc", got)
	}
	if got := hs.GetString("missing"); got != "" {
		t.Fatalf("GetString missing=%q want empty", got)
	}
	if got := hs.GetInt(HeaderSum); got != 3 {
		t.Fatalf("GetInt sum=%d want 3", got)
	}
	if got := hs.GetInt(HeaderType); got != 0 { // non-numeric value → 0
		t.Fatalf("GetInt type=%d want 0", got)
	}
	hs = appendHeader(hs, HeaderBizRt, "42")
	if got := hs.GetString(HeaderBizRt); got != "42" {
		t.Fatalf("GetString after Add=%q want 42", got)
	}
}

// TestNewPingFrame verifies the ping carries the service id and type header.
func TestNewPingFrame(t *testing.T) {
	f := NewPingFrame(12345)
	if f.Service != 12345 || f.Method != MethodControl {
		t.Fatalf("ping frame service/method = %d/%d, want 12345/control", f.Service, f.Method)
	}
	if got := Headers(f.Headers).GetString(HeaderType); got != TypePing {
		t.Fatalf("ping type header = %q want %q", got, TypePing)
	}
}

// TestNewAckFrame_ClonesHeaders verifies the ACK inherits the inbound headers
// (so the server can correlate the ack to the original delivery) and replaces
// only the payload.
func TestNewAckFrame_ClonesHeaders(t *testing.T) {
	in := Frame{
		SeqID:   1,
		LogID:   2,
		Service: 3,
		Method:  MethodData,
		Headers: Headers{{Key: HeaderMessageID, Value: "om_x"}, {Key: HeaderType, Value: TypeEvent}},
	}
	ack := NewAckFrame(in, []byte(`{"code":200}`))
	if ack.SeqID != 1 || ack.LogID != 2 || ack.Service != 3 || ack.Method != MethodData {
		t.Fatalf("ack identity not inherited: %+v", ack)
	}
	if !bytes.Equal(ack.Payload, []byte(`{"code":200}`)) {
		t.Fatalf("ack payload = %q", ack.Payload)
	}
	if got := Headers(ack.Headers).GetString(HeaderMessageID); got != "om_x" {
		t.Fatalf("ack header message_id = %q want om_x", got)
	}
	// Mutating ack headers must not bleed into the inbound frame (independent storage).
	mutatedAck := appendHeader(Headers(ack.Headers), "x", "y")
	if got := len(in.Headers); got != 2 {
		t.Fatalf("mutating ack headers bled into inbound: %d (mutated=%v)", got, mutatedAck)
	}
}

// framesEqual compares two frames field-by-field, treating nil and empty
// Headers/Payload as equal — the only divergence between our codec and the
// SDK's: the SDK normalises an absent Payload to []byte{} and an absent
// Headers to []Header{} on decode, while our zero value leaves them nil.
func framesEqual(a, b Frame) bool {
	if a.SeqID != b.SeqID || a.LogID != b.LogID || a.Service != b.Service || a.Method != b.Method {
		return false
	}
	if a.PayloadEncoding != b.PayloadEncoding || a.PayloadType != b.PayloadType || a.LogIDNew != b.LogIDNew {
		return false
	}
	if !bytes.Equal(payloadOrEmpty(a.Payload), payloadOrEmpty(b.Payload)) {
		return false
	}
	return reflect.DeepEqual(headersOrEmpty(a.Headers), headersOrEmpty(b.Headers))
}

func payloadOrEmpty(p []byte) []byte {
	if p == nil {
		return []byte{}
	}
	return p
}

func headersOrEmpty(h []Header) []Header {
	if h == nil {
		return []Header{}
	}
	return h
}
