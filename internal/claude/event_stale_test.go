//go:build linux || darwin

package claude

import (
	"strings"
	"testing"
)

func TestIsStaleSession_TrueOnStaleSessionError(t *testing.T) {
	// Real CLI 2.1.206 shape: result line, is_error, errors[] folded
	// into Event.Result by the parser.
	line := `{"type":"result","subtype":"error","is_error":true,"errors":["No conversation found with session ID: abc-123"],"session_id":"abc-123"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %+v", got)
	}
	if !IsStaleSession(got[0]) {
		t.Errorf("IsStaleSession = false, want true for %+v", got[0])
	}
}

func TestIsStaleSession_FalseOnSuccessResult(t *testing.T) {
	ev := Event{Type: EventResult, Subtype: "success", Result: "all good"}
	if IsStaleSession(ev) {
		t.Errorf("IsStaleSession = true on success result %+v", ev)
	}
}

func TestIsStaleSession_FalseOnNonResultEvent(t *testing.T) {
	ev := Event{Type: EventText, Text: "No conversation found", IsError: true}
	if IsStaleSession(ev) {
		t.Errorf("IsStaleSession = true on non-result event %+v", ev)
	}
}

func TestIsStaleSession_FalseOnUnrelatedErrorText(t *testing.T) {
	ev := Event{Type: EventResult, IsError: true, Result: "rate limit exceeded"}
	if IsStaleSession(ev) {
		t.Errorf("IsStaleSession = true on unrelated error %+v", ev)
	}
	if strings.Contains(ev.Result, "No conversation found") {
		t.Fatal("test premise broken: unrelated error contains stale marker")
	}
}
