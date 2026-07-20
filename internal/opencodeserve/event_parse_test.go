package opencodeserve

import "testing"

func TestParseEventLine_EmptyAndGarbage(t *testing.T) {
	if _, ok := parseEventLine(""); ok {
		t.Fatal("blank line should be dropped")
	}
	if _, ok := parseEventLine("   "); ok {
		t.Fatal("whitespace line should be dropped")
	}
	ev, ok := parseEventLine("{not json")
	if !ok {
		t.Fatal("garbage should be returned as EventError (ok=true) so a malformed stream surfaces")
	}
	if ev.kind != EventError || !ev.isError {
		t.Fatalf("garbage: want EventError, got %+v", ev)
	}
}

func TestParseEventLine_UnknownTypeDropped(t *testing.T) {
	for _, line := range []string{
		`{"type":"server.heartbeat","properties":{}}`,
		`{"type":"plugin.added","properties":{"id":"anthropic"}}`,
		`{"type":"catalog.updated","properties":{}}`,
		`{"type":"reference.updated","properties":{}}`,
		`{"type":"integration.updated","properties":{}}`,
	} {
		if _, ok := parseEventLine(line); ok {
			t.Fatalf("noise frame should be dropped: %s", line)
		}
	}
}

func TestParseEventLine_SessionCreated(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "session.created",
		"properties": {
			"sessionID": "ses_abc",
			"info": {"id": "ses_abc", "model": {"id": "glm-5.2", "providerID": "zhipuai"}}
		}
	}`)
	if !ok {
		t.Fatal("session.created should yield an event")
	}
	if ev.kind != EventSession {
		t.Errorf("kind = %q, want %q", ev.kind, EventSession)
	}
	if ev.sessionID != "ses_abc" {
		t.Errorf("sessionID = %q", ev.sessionID)
	}
	if ev.text != "glm-5.2" {
		t.Errorf("text (model) = %q, want glm-5.2", ev.text)
	}
}

func TestParseEventLine_StepStart(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.updated",
		"properties": {"sessionID": "ses_1", "part": {"type": "step-start"}}
	}`)
	if !ok || ev.kind != EventStepStart {
		t.Fatalf("step-start: got %+v ok=%v", ev, ok)
	}
}

func TestParseEventLine_StepFinishNonStop(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.updated",
		"properties": {"sessionID": "ses_1", "part": {
			"type": "step-finish", "reason": "tool-calls",
			"tokens": {"total": 100, "input": 10, "output": 5, "cache": {"read": 80, "write": 5}},
			"cost": 0.001
		}}
	}`)
	if !ok || ev.kind != EventStepFinish {
		t.Fatalf("non-stop step-finish: got %+v ok=%v", ev, ok)
	}
	if ev.inputTokens != 10 || ev.outputTokens != 5 {
		t.Errorf("tokens: in=%d out=%d", ev.inputTokens, ev.outputTokens)
	}
	if ev.cacheRead != 80 || ev.cacheWrite != 5 {
		t.Errorf("cache: r=%d w=%d", ev.cacheRead, ev.cacheWrite)
	}
	if ev.cost != 0.001 {
		t.Errorf("cost = %v", ev.cost)
	}
}

func TestParseEventLine_StepFinishStopIsResult(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.updated",
		"properties": {"sessionID": "ses_1", "part": {
			"type": "step-finish", "reason": "stop",
			"tokens": {"input": 30, "output": 12, "cache": {"read": 100, "write": 0}},
			"cost": 0.002
		}}
	}`)
	if !ok || ev.kind != EventResult {
		t.Fatalf("stop step-finish should be EventResult: %+v ok=%v", ev, ok)
	}
	if ev.inputTokens != 30 || ev.outputTokens != 12 {
		t.Errorf("tokens: in=%d out=%d", ev.inputTokens, ev.outputTokens)
	}
}

func TestParseEventLine_ToolUsePending(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.updated",
		"properties": {"sessionID": "ses_1", "part": {
			"type": "tool", "tool": "bash",
			"state": {"status": "pending", "input": {"command": "ls"}, "raw": ""}
		}}
	}`)
	if !ok || ev.kind != EventToolUse {
		t.Fatalf("pending tool: got %+v ok=%v", ev, ok)
	}
	if ev.toolName != "bash" {
		t.Errorf("toolName = %q", ev.toolName)
	}
	if ev.toolInput == "" || ev.toolInput == "null" {
		t.Errorf("toolInput should carry the input doc: %q", ev.toolInput)
	}
	if !contains(ev.toolInput, "command") {
		t.Errorf("toolInput lost the input fields: %q", ev.toolInput)
	}
}

func TestParseEventLine_ToolResultCompleted(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.updated",
		"properties": {"sessionID": "ses_1", "part": {
			"type": "tool", "tool": "bash",
			"state": {"status": "completed", "output": {"stdout": "hello"}}
		}}
	}`)
	if !ok || ev.kind != EventToolResult {
		t.Fatalf("completed tool: got %+v ok=%v", ev, ok)
	}
	if ev.isToolError {
		t.Errorf("completed tool must not be flagged error")
	}
	if !contains(ev.text, "hello") {
		t.Errorf("tool output lost: %q", ev.text)
	}
}

func TestParseEventLine_ToolResultFailed(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.updated",
		"properties": {"sessionID": "ses_1", "part": {
			"type": "tool", "tool": "bash",
			"state": {"status": "failed", "error": {"message": "boom"}}
		}}
	}`)
	if !ok || ev.kind != EventToolResult {
		t.Fatalf("failed tool: got %+v ok=%v", ev, ok)
	}
	if !ev.isToolError {
		t.Errorf("failed tool must be flagged error")
	}
	if !contains(ev.text, "boom") {
		t.Errorf("error output lost: %q", ev.text)
	}
}

func TestParseEventLine_PartDeltaText(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.delta",
		"properties": {"sessionID": "ses_1", "field": "text", "delta": "Hello"}
	}`)
	if !ok || ev.kind != EventText {
		t.Fatalf("text delta: got %+v ok=%v", ev, ok)
	}
	if ev.text != "Hello" {
		t.Errorf("text = %q", ev.text)
	}
}

func TestParseEventLine_PartDeltaReasoning(t *testing.T) {
	ev, ok := parseEventLine(`{
		"type": "message.part.delta",
		"properties": {"sessionID": "ses_1", "field": "reasoning", "delta": "thinking"}
	}`)
	if !ok || ev.kind != EventThinking {
		t.Fatalf("reasoning delta: got %+v ok=%v", ev, ok)
	}
}

func TestParseEventLine_PartDeltaUnknownDropped(t *testing.T) {
	if _, ok := parseEventLine(`{
		"type": "message.part.delta",
		"properties": {"sessionID": "ses_1", "field": "metadata", "delta": "x"}
	}`); ok {
		t.Fatal("unknown delta field should be dropped")
	}
}

func TestParseEventLine_PartUpdatedTextDropped(t *testing.T) {
	// message.part.updated with a text part is the initial full-text snapshot
	// of the user's input echo (or a re-render after edits). Deltas carry the
	// assistant text incrementally, so the snapshot is redundant.
	if _, ok := parseEventLine(`{
		"type": "message.part.updated",
		"properties": {"sessionID": "ses_1", "part": {"type": "text", "text": "user input"}}
	}`); ok {
		t.Fatal("part.updated[type=text] should be dropped (redundant with deltas)")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
