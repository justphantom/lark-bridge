//go:build linux || darwin

package claude

import (
	"testing"
)

// TestParseEvent_ResultStopReasonAndDurationAPI verifies the CLI 2.1.206
// result-line metadata (stop_reason, duration_api_ms) reaches the Event.
func TestParseEvent_ResultStopReasonAndDurationAPI(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","duration_api_ms":1234,"duration_ms":1500,"result":"ok","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.StopReason != "end_turn" {
		t.Errorf("StopReason = %q, want end_turn", ev.StopReason)
	}
	if ev.DurationAPIMs != 1234 {
		t.Errorf("DurationAPIMs = %d, want 1234", ev.DurationAPIMs)
	}
	if ev.DurationMs != 1500 {
		t.Errorf("DurationMs = %d, want 1500", ev.DurationMs)
	}
}
