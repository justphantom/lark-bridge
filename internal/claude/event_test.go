//go:build linux || darwin

package claude

import (
	"strings"
	"testing"
)

func TestParseEvent_EmptyAndGarbage(t *testing.T) {
	if got, err := parseEvent("   "); err != nil || len(got) != 0 {
		t.Fatalf("empty line: got %v, err %v", got, err)
	}
	if got, err := parseEvent(""); err != nil || len(got) != 0 {
		t.Fatalf("blank line: got %v, err %v", got, err)
	}
	if _, err := parseEvent("{not json"); err == nil {
		t.Fatalf("garbage: want error, got nil")
	}
}

func TestParseEvent_SystemInit(t *testing.T) {
	line := `{"type":"system","subtype":"init","cwd":"/tmp","session_id":"abc-123","tools":["Bash"],"model":"claude-x"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventSystem {
		t.Fatalf("got %+v", got)
	}
	if got[0].SessionID != "abc-123" {
		t.Errorf("session_id = %q", got[0].SessionID)
	}
	if got[0].Model != "claude-x" {
		t.Errorf("model = %q", got[0].Model)
	}
	if got[0].Subtype != "init" {
		t.Errorf("subtype = %q", got[0].Subtype)
	}
	if got[0].Raw != line {
		t.Errorf("raw not retained")
	}
}

func TestParseEvent_ResultSuccess(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.0123,"result":"Final answer","session_id":"abc-123"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.Result != "Final answer" {
		t.Errorf("result = %q", ev.Result)
	}
	if ev.IsError {
		t.Errorf("is_error should be false")
	}
	if ev.CostUSD != 0.0123 {
		t.Errorf("cost = %v", ev.CostUSD)
	}
	if ev.DurationMs != 1234 {
		t.Errorf("duration = %d", ev.DurationMs)
	}
	if ev.SessionID != "abc-123" {
		t.Errorf("session_id = %q", ev.SessionID)
	}
}

func TestParseEvent_ResultError(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"result":"boom","session_id":"s1"}`
	got, _ := parseEvent(line)
	if got[0].Type != EventResult || !got[0].IsError || got[0].Result != "boom" {
		t.Fatalf("got %+v", got[0])
	}
}

func TestParseEvent_ResultWithUsage(t *testing.T) {
	line := `{
		"type":"result",
		"subtype":"success",
		"session_id":"s1",
		"duration_ms":1234,
		"total_cost_usd":0.0123,
		"usage": {
			"input_tokens": 1000,
			"cache_creation_input_tokens": 200,
			"cache_read_input_tokens": 300,
			"output_tokens": 500
		},
		"num_turns": 3,
		"result":"ok"
	}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.InputTokens != 1000 {
		t.Errorf("input_tokens = %d, want 1000", ev.InputTokens)
	}
	if ev.OutputTokens != 500 {
		t.Errorf("output_tokens = %d, want 500", ev.OutputTokens)
	}
}

func TestParseEvent_ResultLenientOnBadNumeric(t *testing.T) {
	// A malformed numeric field (total_cost_usd as a string) fails the
	// strict lineHead decode. The lenient path must still surface the
	// final answer so the user is never left without a reply; numeric
	// accounting falls back to zero.
	line := `{"type":"result","subtype":"success","is_error":false,"total_cost_usd":"oops","duration_ms":1234,"result":"Final answer","session_id":"abc-123"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.Result != "Final answer" {
		t.Errorf("result = %q, want %q", ev.Result, "Final answer")
	}
	if ev.SessionID != "abc-123" {
		t.Errorf("session_id = %q", ev.SessionID)
	}
	if ev.CostUSD != 0 || ev.DurationMs != 0 {
		t.Errorf("lenient numeric fields should be zero: cost=%v dur=%d", ev.CostUSD, ev.DurationMs)
	}
}

func TestParseEvent_AssistantText(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"Hello!"}]},"session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventText || got[0].Text != "Hello!" {
		t.Fatalf("got %+v", got)
	}
	if got[0].SessionID != "s1" {
		t.Errorf("session_id = %q", got[0].SessionID)
	}
}

func TestParseEvent_AssistantToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la"}}]},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].Type != EventToolUse {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.ToolName != "Bash" {
		t.Errorf("tool name = %q", ev.ToolName)
	}
	if ev.ToolID != "toolu_1" {
		t.Errorf("tool id = %q", ev.ToolID)
	}
	if !strings.Contains(ev.ToolInput, "ls -la") {
		t.Errorf("tool input = %q", ev.ToolInput)
	}
}

func TestParseEvent_ToolResultStringContent(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"total 0","is_error":false}]},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].Type != EventToolResult {
		t.Fatalf("got %+v", got)
	}
	if got[0].ToolID != "toolu_1" || got[0].Text != "total 0" || got[0].IsToolError {
		t.Fatalf("got %+v", got[0])
	}
}

func TestParseEvent_ToolResultArrayContent(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"file contents"}],"is_error":true}]},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].Type != EventToolResult {
		t.Fatalf("got %+v", got)
	}
	if got[0].Text != "file contents" {
		t.Errorf("text = %q", got[0].Text)
	}
	if !got[0].IsToolError {
		t.Errorf("is_tool_error should be true")
	}
	if got[0].ToolID != "toolu_2" {
		t.Errorf("ToolID = %q, want toolu_2", got[0].ToolID)
	}
}

func TestParseEvent_MultiBlockAssistant(t *testing.T) {
	line := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Running "},{"type":"tool_use","id":"toolu_3","name":"Bash","input":{"command":"pwd"}}]},"session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 events, got %d (%+v)", len(got), got)
	}
	if got[0].Type != EventText || got[0].Text != "Running " {
		t.Errorf("first block = %+v", got[0])
	}
	if got[1].Type != EventToolUse || got[1].ToolName != "Bash" {
		t.Errorf("second block = %+v", got[1])
	}
}

func TestParseEvent_UnknownTypeForwarded(t *testing.T) {
	line := `{"type":"future_event","subtype":"x","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != "future_event" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseEvent_AssistantNoMessage(t *testing.T) {
	// An assistant line missing the message envelope should not crash;
	// it yields no events (defensive nil guard).
	line := `{"type":"assistant","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 events, got %+v", got)
	}
}

func TestParseEvent_AssistantThinking(t *testing.T) {
	// Real CLI 2.1.206 shape: reasoning lives in "thinking", not "text".
	line := `{"type":"assistant","message":{"id":"msg_4","role":"assistant","content":[{"type":"thinking","thinking":"let me think","signature":"sig"}]},"session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventThinking {
		t.Fatalf("got %+v", got)
	}
	if got[0].Text != "let me think" {
		t.Errorf("thinking text = %q, want %q", got[0].Text, "let me think")
	}
	if got[0].SessionID != "s1" {
		t.Errorf("session_id = %q", got[0].SessionID)
	}
}

func TestParseEvent_ThinkingTokens(t *testing.T) {
	line := `{"type":"system","subtype":"thinking_tokens","estimated_tokens":1024,"estimated_tokens_delta":256,"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].Type != EventSystem || got[0].Subtype != "thinking_tokens" {
		t.Fatalf("got %+v", got)
	}
}
