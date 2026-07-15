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
	if len(got) != 1 || got[0].kind != EventSystem {
		t.Fatalf("got %+v", got)
	}
	if got[0].sessionID != "abc-123" {
		t.Errorf("session_id = %q", got[0].sessionID)
	}
	if got[0].model != "claude-x" {
		t.Errorf("model = %q", got[0].model)
	}
	if got[0].subtype != "init" {
		t.Errorf("subtype = %q", got[0].subtype)
	}
	if got[0].raw != line {
		t.Errorf("raw not retained")
	}
}

func TestParseEvent_ResultSuccess(t *testing.T) {
	line := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1234,"total_cost_usd":0.0123,"result":"Final answer","session_id":"abc-123"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.result != "Final answer" {
		t.Errorf("result = %q", ev.result)
	}
	if ev.isError {
		t.Errorf("is_error should be false")
	}
	if ev.costUSD != 0.0123 {
		t.Errorf("cost = %v", ev.costUSD)
	}
	if ev.durationMs != 1234 {
		t.Errorf("duration = %d", ev.durationMs)
	}
	if ev.sessionID != "abc-123" {
		t.Errorf("session_id = %q", ev.sessionID)
	}
}

func TestParseEvent_ResultError(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,"result":"boom","session_id":"s1"}`
	got, _ := parseEvent(line)
	if got[0].kind != EventResult || !got[0].isError || got[0].result != "boom" {
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
	if len(got) != 1 || got[0].kind != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.inputTokens != 1000 {
		t.Errorf("input_tokens = %d, want 1000", ev.inputTokens)
	}
	if ev.outputTokens != 500 {
		t.Errorf("output_tokens = %d, want 500", ev.outputTokens)
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
	if len(got) != 1 || got[0].kind != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.result != "Final answer" {
		t.Errorf("result = %q, want %q", ev.result, "Final answer")
	}
	if ev.sessionID != "abc-123" {
		t.Errorf("session_id = %q", ev.sessionID)
	}
	if ev.costUSD != 0 || ev.durationMs != 0 {
		t.Errorf("lenient numeric fields should be zero: cost=%v dur=%d", ev.costUSD, ev.durationMs)
	}
}

func TestParseEvent_AssistantText(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"Hello!"}]},"session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventText || got[0].text != "Hello!" {
		t.Fatalf("got %+v", got)
	}
	if got[0].sessionID != "s1" {
		t.Errorf("session_id = %q", got[0].sessionID)
	}
}

func TestParseEvent_AssistantToolUse(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls -la"}}]},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].kind != EventToolUse {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.toolName != "Bash" {
		t.Errorf("tool name = %q", ev.toolName)
	}
	if ev.toolID != "toolu_1" {
		t.Errorf("tool id = %q", ev.toolID)
	}
	if ev.GetToolID() != "toolu_1" {
		t.Errorf("GetToolID = %q, want toolu_1", ev.GetToolID())
	}
	if !strings.Contains(ev.toolInput, "ls -la") {
		t.Errorf("tool input = %q", ev.toolInput)
	}
}

func TestParseEvent_ToolResultStringContent(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"total 0","is_error":false}]},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].kind != EventToolResult {
		t.Fatalf("got %+v", got)
	}
	if got[0].toolID != "toolu_1" || got[0].text != "total 0" || got[0].isToolError {
		t.Fatalf("got %+v", got[0])
	}
}

func TestParseEvent_ToolResultArrayContent(t *testing.T) {
	line := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"file contents"}],"is_error":true}]},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].kind != EventToolResult {
		t.Fatalf("got %+v", got)
	}
	if got[0].text != "file contents" {
		t.Errorf("text = %q", got[0].text)
	}
	if !got[0].isToolError {
		t.Errorf("is_tool_error should be true")
	}
	if got[0].GetToolID() != "toolu_2" {
		t.Errorf("GetToolID = %q, want toolu_2", got[0].GetToolID())
	}
	if !got[0].GetIsToolError() {
		t.Errorf("GetIsToolError should be true")
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
	if got[0].kind != EventText || got[0].text != "Running " {
		t.Errorf("first block = %+v", got[0])
	}
	if got[1].kind != EventToolUse || got[1].toolName != "Bash" {
		t.Errorf("second block = %+v", got[1])
	}
}

func TestParseEvent_UnknownTypeForwarded(t *testing.T) {
	line := `{"type":"future_event","subtype":"x","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != "future_event" {
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

func TestParseEvent_ThinkingTokens(t *testing.T) {
	line := `{"type":"system","subtype":"thinking_tokens","estimated_tokens":1024,"estimated_tokens_delta":256,"session_id":"s1"}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].kind != EventSystem || got[0].subtype != "thinking_tokens" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseEvent_TaskStarted(t *testing.T) {
	line := `{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu_1","description":"Explore codebase architecture","subagent_type":"Explore","task_type":"local_agent","prompt":"...","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventTaskStarted {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.taskType != "Explore" {
		t.Errorf("taskType = %q, want Explore", ev.taskType)
	}
	if ev.taskDesc != "Explore codebase architecture" {
		t.Errorf("taskDesc = %q", ev.taskDesc)
	}
	if ev.GetTaskType() != "Explore" {
		t.Errorf("GetTaskType = %q", ev.GetTaskType())
	}
}

func TestParseEvent_TaskProgress(t *testing.T) {
	// task_progress carries a live description (changes per tick) plus
	// cumulative usage. These fields drive the subagent tool-row updates.
	line := `{"type":"system","subtype":"task_progress","task_id":"t1","tool_use_id":"tu_1","description":"Reading internal/opencode/model.go","subagent_type":"Explore","usage":{"total_tokens":104609,"tool_uses":65,"duration_ms":59675},"last_tool_name":"Read","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventTaskProgress {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.taskDesc != "Reading internal/opencode/model.go" {
		t.Errorf("taskDesc = %q", ev.taskDesc)
	}
	if ev.taskTokens != 104609 {
		t.Errorf("taskTokens = %d, want 104609", ev.taskTokens)
	}
	if ev.taskSteps != 65 {
		t.Errorf("taskSteps = %d, want 65", ev.taskSteps)
	}
	if ev.taskMs != 59675 {
		t.Errorf("taskMs = %d, want 59675", ev.taskMs)
	}
}

func TestParseEvent_TaskNotification(t *testing.T) {
	// task_notification carries the terminal summary (not description) and
	// marks non-completed status as an error so the row renders ❌.
	line := `{"type":"system","subtype":"task_notification","task_id":"t1","tool_use_id":"tu_1","status":"completed","output_file":"","summary":"Explore codebase architecture","usage":{"total_tokens":107296,"tool_uses":66,"duration_ms":98342},"session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventTaskNotification {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.taskDesc != "Explore codebase architecture" {
		t.Errorf("taskDesc = %q (should come from summary)", ev.taskDesc)
	}
	if ev.isToolError {
		t.Errorf("completed status should not be an error")
	}
	if ev.taskSteps != 66 || ev.taskMs != 98342 {
		t.Errorf("usage = steps %d ms %d", ev.taskSteps, ev.taskMs)
	}
}

func TestParseEvent_TaskNotificationFailed(t *testing.T) {
	line := `{"type":"system","subtype":"task_notification","task_id":"t1","status":"failed","summary":"boom","usage":{"total_tokens":10,"tool_uses":1,"duration_ms":100},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if !got[0].isToolError {
		t.Errorf("non-completed status should flag isToolError")
	}
}
