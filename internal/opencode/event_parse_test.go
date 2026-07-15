package opencode

import (
	"strings"
	"testing"
)

func TestParseEvent_EmptyAndGarbage(t *testing.T) {
	if got, err := parseEvent("   "); err != nil || len(got) != 0 {
		t.Fatalf("whitespace line: got %v, err %v", got, err)
	}
	if got, err := parseEvent(""); err != nil || len(got) != 0 {
		t.Fatalf("blank line: got %v, err %v", got, err)
	}
	if _, err := parseEvent("{not json"); err == nil {
		t.Fatalf("garbage: want error, got nil")
	}
}

func TestParseEvent_SessionCreated(t *testing.T) {
	line := `{"type":"session.created","sessionID":"sess-1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventSession {
		t.Fatalf("got %+v", got)
	}
	if got[0].sessionID != "sess-1" {
		t.Errorf("sessionID = %q", got[0].sessionID)
	}
	if got[0].raw != line {
		t.Errorf("raw not retained")
	}
}

func TestParseEvent_StepStart(t *testing.T) {
	line := `{"type":"step_start","sessionID":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventStepStart {
		t.Fatalf("got %+v", got)
	}
	if got[0].sessionID != "s1" {
		t.Errorf("sessionID = %q", got[0].sessionID)
	}
}

func TestParseEvent_Text(t *testing.T) {
	line := `{"type":"text","sessionID":"s1","part":{"type":"text","text":"Hello!"}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventText {
		t.Fatalf("got %+v", got)
	}
	if got[0].text != "Hello!" {
		t.Errorf("text = %q", got[0].text)
	}
}

func TestParseEvent_Reasoning(t *testing.T) {
	line := `{"type":"reasoning","sessionID":"s1","part":{"type":"reasoning","text":"thinking hard"}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventThinking {
		t.Fatalf("got %+v", got)
	}
	if got[0].text != "thinking hard" {
		t.Errorf("text = %q", got[0].text)
	}
}

func TestParseEvent_ToolUseCompleted(t *testing.T) {
	// opencode emits one completed tool_use line; the parser produces a
	// single EventToolResult carrying both the input summary (toolInput)
	// and the output, so the card shows "Read: README.md" + output.
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"read","title":"README.md","state":{"status":"completed","output":"file contents"}}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 result event, got %d: %+v", len(got), got)
	}
	res := got[0]
	if res.kind != EventToolResult || res.toolName != "read" || res.text != "file contents" || res.isToolError {
		t.Errorf("result event = %+v", res)
	}
	if res.toolInput != "README.md" {
		t.Errorf("toolInput = %q, want README.md", res.toolInput)
	}
}

func TestParseEvent_ToolUseError(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"bash","state":{"status":"error","output":"exit 1"}}}`
	got, _ := parseEvent(line)
	if len(got) != 1 {
		t.Fatalf("want 1 result event, got %d: %+v", len(got), got)
	}
	res := got[0]
	if res.kind != EventToolResult || !res.isToolError {
		t.Errorf("error result should have isToolError, got %+v", res)
	}
	if !res.GetIsToolError() {
		t.Error("GetIsToolError should be true")
	}
}

func TestParseEvent_ToolUseRunning(t *testing.T) {
	// A running tool_use still yields a single EventToolResult (not flagged
	// as error): opencode's per-call event already carries the terminal
	// status, so a "running" status is treated as in-progress-but-ok.
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"bash","state":{"status":"running"}}}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].kind != EventToolResult {
		t.Fatalf("want 1 ToolResult event, got %+v", got)
	}
	if got[0].isToolError {
		t.Errorf("running status should not be an error")
	}
}

func TestParseEvent_ToolUseInputFallback(t *testing.T) {
	// When part.title is empty, the tool input falls back to state.input JSON.
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"ls"}}}}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].kind != EventToolResult {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got[0].toolInput, "ls") {
		t.Errorf("input fallback = %q, want to contain ls", got[0].toolInput)
	}
}

func TestParseEvent_StepFinishStop(t *testing.T) {
	// reason="stop" is terminal: produces an EventResult with token/cost.
	line := `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":1500,"input":1000,"output":500,"cache":{"read":300,"write":50}},"cost":0.02}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventResult {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.GetInputTokens() != 1000 {
		t.Errorf("inputTokens = %d, want 1000", ev.GetInputTokens())
	}
	if ev.GetOutputTokens() != 500 {
		t.Errorf("outputTokens = %d, want 500", ev.GetOutputTokens())
	}
	if ev.GetCacheRead() != 300 {
		t.Errorf("cacheRead = %d, want 300", ev.GetCacheRead())
	}
	if ev.GetCacheWrite() != 50 {
		t.Errorf("cacheWrite = %d, want 50", ev.GetCacheWrite())
	}
	if ev.cost != 0.02 {
		t.Errorf("cost = %v", ev.cost)
	}
	if ev.GetCost() != 0.02 {
		t.Errorf("GetCost = %v", ev.GetCost())
	}
}

func TestParseEvent_StepFinishToolCalls(t *testing.T) {
	// reason="tool-calls" is NOT terminal, but it still carries this step's
	// token accounting. It is surfaced as an EventStepFinish so the bridge
	// can accumulate the full turn total; previously these steps were
	// dropped, losing ~96% of input tokens on tool-heavy turns.
	line := `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"tool-calls","tokens":{"total":800,"input":200,"output":80,"cache":{"read":400,"write":0}},"cost":0}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventStepFinish {
		t.Fatalf("tool-calls should produce one EventStepFinish, got %+v", got)
	}
	ev := got[0]
	if ev.GetInputTokens() != 200 {
		t.Errorf("inputTokens = %d, want 200", ev.GetInputTokens())
	}
	if ev.GetOutputTokens() != 80 {
		t.Errorf("outputTokens = %d, want 80", ev.GetOutputTokens())
	}
	if ev.GetCacheRead() != 400 {
		t.Errorf("cacheRead = %d, want 400", ev.GetCacheRead())
	}
	if ev.GetCacheWrite() != 0 {
		t.Errorf("cacheWrite = %d, want 0", ev.GetCacheWrite())
	}
}

func TestParseEvent_ErrorWithMessage(t *testing.T) {
	line := `{"type":"error","sessionID":"s1","message":"something broke"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventError {
		t.Fatalf("got %+v", got)
	}
	if got[0].text != "something broke" {
		t.Errorf("text = %q", got[0].text)
	}
	if !got[0].isError {
		t.Error("isError should be true")
	}
}

func TestParseEvent_ErrorFallbackField(t *testing.T) {
	// When "message" is empty, fall back to the "error" field.
	line := `{"type":"error","sessionID":"s1","error":"err field msg"}`
	got, _ := parseEvent(line)
	if got[0].text != "err field msg" {
		t.Errorf("text = %q", got[0].text)
	}
}

func TestParseEvent_UnknownTypeForwarded(t *testing.T) {
	line := `{"type":"future_event","sessionID":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != "future_event" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseEvent_MalformedPartReturnsError(t *testing.T) {
	// A present-but-invalid part must surface an error (M8: no silent swallow),
	// so pump can log the schema drift instead of emitting an empty event.
	line := `{"type":"text","sessionID":"s1","part":{not valid json}}`
	if _, err := parseEvent(line); err == nil {
		t.Fatal("expected error for malformed part, got nil")
	}
}

func TestParseEvent_NoPartIsFine(t *testing.T) {
	// Event types that do not carry a part must parse without error.
	line := `{"type":"step_start","sessionID":"s1"}`
	if _, err := parseEvent(line); err != nil {
		t.Fatalf("unexpected error when part is absent: %v", err)
	}
}

func TestStringifyContent(t *testing.T) {
	// plain string
	if got := stringifyContent([]byte(`"hello"`)); got != "hello" {
		t.Errorf("string = %q", got)
	}
	// content-block array
	arr := []byte(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
	if got := stringifyContent(arr); got != "ab" {
		t.Errorf("array = %q", got)
	}
	// empty
	if got := stringifyContent(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
}

func TestStringifyJSON(t *testing.T) {
	// compacted JSON
	if got := stringifyJSON([]byte(`{"a": 1}`)); got != `{"a":1}` {
		t.Errorf("compact = %q", got)
	}
	if got := stringifyJSON(nil); got != "" {
		t.Errorf("empty = %q", got)
	}
}
