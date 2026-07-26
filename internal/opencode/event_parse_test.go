//go:build linux || darwin

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

// TestParseEvent_ToolUseBashNonZeroExit verifies the bash-failure detection:
// opencode sets state.status="completed" even when the command exits
// non-zero, so the parser must read state.metadata.exit and flag the result
// as an error when exit!=0. Without this, `cat /nonexistent` looks
// identical to a successful `ls` on the card.
func TestParseEvent_ToolUseBashNonZeroExit(t *testing.T) {
	// Shape mirrors a real opencode 1.18 line for `cat /nonexistent`:
	// status=completed, exit=1, output carries the stderr text.
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"cat /nonexistent"},"output":"cat: /nonexistent: No such file or directory\n","metadata":{"output":"cat: /nonexistent: No such file or directory\n","exit":1,"truncated":false},"title":"cat /nonexistent"}}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d: %+v", len(got), got)
	}
	res := got[0]
	if res.kind != EventToolResult {
		t.Fatalf("kind = %q, want EventToolResult", res.kind)
	}
	if !res.isToolError {
		t.Errorf("expected isToolError=true for exit!=0, got %+v", res)
	}
	if !res.GetIsToolError() {
		t.Error("GetIsToolError should be true")
	}
	if !strings.Contains(res.text, "No such file") {
		t.Errorf("output not preserved: %q", res.text)
	}
}

// TestParseEvent_ToolUseBashZeroExitNotError verifies exit==0 (the default
// when metadata is absent or explicitly 0) does NOT flag an error — guards
// against a regression where the new exit check accidentally fires on
// tools that do not populate metadata (read/write/edit/...).
func TestParseEvent_ToolUseBashZeroExitNotError(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"file1\nfile2\n","metadata":{"output":"file1\nfile2\n","exit":0,"truncated":false},"title":"ls"}}}`
	got, _ := parseEvent(line)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if got[0].isToolError {
		t.Errorf("exit=0 must not be flagged as error, got %+v", got[0])
	}
}

// TestParseEvent_ToolUseNoMetadataNotError verifies a tool result without
// any metadata block (e.g. read/write) is unaffected by the new exit check.
func TestParseEvent_ToolUseNoMetadataNotError(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"read","title":"README.md","state":{"status":"completed","output":"contents"}}}`
	got, _ := parseEvent(line)
	if len(got) != 1 || got[0].isToolError {
		t.Errorf("read without metadata must not be an error, got %+v", got)
	}
}

// TestParseEvent_ToolUseErrorFieldSurfaced verifies that when status="error"
// opencode swaps state.output for state.error, and the parser surfaces the
// cause ("File not found", "PermissionDenied", ...) as the result text —
// without this the card shows an empty body and only the red tag.
func TestParseEvent_ToolUseErrorFieldSurfaced(t *testing.T) {
	// Shape mirrors a real opencode 1.18 read of a missing file:
	// state.output is absent, state.error carries the cause.
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"read","callID":"call_x","state":{"status":"error","input":{"filePath":"/tmp/missing"},"error":"File not found: /tmp/missing","time":{"start":1785051111461,"end":1785051111652}},"id":"prt_x","sessionID":"s1","messageID":"msg_x"}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	res := got[0]
	if !res.isToolError {
		t.Errorf("expected isToolError=true for status=error")
	}
	if res.text != "File not found: /tmp/missing" {
		t.Errorf("text = %q, want the cause from state.error", res.text)
	}
	if !strings.Contains(res.GetToolInput(), "/tmp/missing") {
		// input fallback still works (no title here so input JSON is used)
		t.Errorf("toolInput = %q, want contains the file path", res.GetToolInput())
	}
}

// TestParseEvent_ToolUseErrorFieldOnWrite verifies the same state.error
// surfacing for write failures (PermissionDenied).
func TestParseEvent_ToolUseErrorFieldOnWrite(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"write","state":{"status":"error","input":{"filePath":"/tmp/readonly/x","content":"hello"},"error":"PermissionDenied: FileSystem.writeFile (/tmp/readonly/x)"}}}`
	got, _ := parseEvent(line)
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	if !got[0].isToolError {
		t.Error("expected isToolError=true")
	}
	if !strings.Contains(got[0].text, "PermissionDenied") {
		t.Errorf("text = %q, want contains PermissionDenied", got[0].text)
	}
}

// TestParseEvent_ToolUseErrorFieldEmptyFallsBackToOutput verifies that when
// status=error but state.error is absent (defensive — older CLI shapes), the
// parser falls back to state.output rather than emitting empty text.
func TestParseEvent_ToolUseErrorFieldEmptyFallsBackToOutput(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"bash","state":{"status":"error","output":"legacy output field"}}}`
	got, _ := parseEvent(line)
	if got[0].text != "legacy output field" {
		t.Errorf("text = %q, want fallback to state.output", got[0].text)
	}
}

// TestParseEvent_EditDiffStatAppended verifies edit results carry a "+N -M"
// suffix on the tool-input summary when state.metadata.filediff is populated.
// The renderer has no structured diff slot today, so the stat rides on the
// input string the tool row already displays.
func TestParseEvent_EditDiffStatAppended(t *testing.T) {
	// Shape mirrors a real opencode 1.18 edit: title is the path, metadata
	// .filediff carries additions/deletions.
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"edit","title":"tmp/x.txt","state":{"status":"completed","input":{"filePath":"/tmp/x.txt","oldString":"a","newString":"b"},"output":"Edit applied successfully.","metadata":{"filediff":{"file":"/tmp/x.txt","additions":3,"deletions":1},"truncated":false}}}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	res := got[0]
	if !strings.Contains(res.toolInput, "tmp/x.txt") {
		t.Errorf("toolInput = %q, want contains the path", res.toolInput)
	}
	if !strings.Contains(res.toolInput, "(+3 -1)") {
		t.Errorf("toolInput = %q, want '(+3 -1)' suffix", res.toolInput)
	}
}

// TestParseEvent_EditNoDiffStatWhenAbsent verifies edit results without a
// filediff block don't get a phantom "(+0 -0)" suffix.
func TestParseEvent_EditNoDiffStatWhenAbsent(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"edit","title":"tmp/x.txt","state":{"status":"completed","output":"ok"}}}`
	got, _ := parseEvent(line)
	if got[0].toolInput != "tmp/x.txt" {
		t.Errorf("toolInput = %q, want bare path when filediff absent", got[0].toolInput)
	}
}

// TestParseEvent_EditZeroDiffOmitted verifies additions=0 AND deletions=0
// does not append a "(+0 -0)" suffix (the case where a no-op edit landed).
func TestParseEvent_EditZeroDiffOmitted(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"edit","title":"tmp/x.txt","state":{"status":"completed","metadata":{"filediff":{"additions":0,"deletions":0}}}}}`
	got, _ := parseEvent(line)
	if strings.Contains(got[0].toolInput, "(+0 -0)") {
		t.Errorf("toolInput = %q, must not append zero diffstat", got[0].toolInput)
	}
}

// TestParseEvent_EditNoTitleFallsBackToFilePath pins the title-absent path:
// opencode does not always populate part.title (edit on some versions),
// and without this fallback the whole input JSON — including oldString/
// newString, often hundreds of runes — would land in the tool-row
// description. The helper extracts filePath/command/.../description in that
// order, mirroring bridgebase.SummarizeToolInput.
func TestParseEvent_EditNoTitleFallsBackToFilePath(t *testing.T) {
	// Same input shape as a real edit (filePath + oldString + newString),
	// but part.title is absent.
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/tmp/x.txt","oldString":"a","newString":"b"},"output":"Edit applied successfully."}}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 event, got %d", len(got))
	}
	res := got[0]
	if res.toolInput != "/tmp/x.txt" {
		t.Errorf("toolInput = %q, want /tmp/x.txt extracted from filePath", res.toolInput)
	}
	// oldString/newString must NOT leak into the tool-row description.
	if strings.Contains(res.toolInput, "oldString") || strings.Contains(res.toolInput, "newString") {
		t.Errorf("toolInput = %q, must not contain oldString/newString", res.toolInput)
	}
}

// TestParseEvent_EditNoTitleDiffStatAppended verifies the title-absent path
// still appends the diffstat suffix after extracting filePath, so the row
// reads "/tmp/x.txt (+3 -1)" — same shape as the title-present path.
func TestParseEvent_EditNoTitleDiffStatAppended(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/tmp/x.txt","oldString":"a","newString":"bbbb"},"output":"ok","metadata":{"filediff":{"additions":3,"deletions":1}}}}}`
	got, _ := parseEvent(line)
	res := got[0]
	if res.toolInput != "/tmp/x.txt (+3 -1)" {
		t.Errorf("toolInput = %q, want \"/tmp/x.txt (+3 -1)\" (filePath + diffstat)", res.toolInput)
	}
}

// TestParseEvent_NoTitleUnknownToolFallsBackToStringifyJSON guards the final
// fallback: an unknown tool shape (no priority field in input) still
// serialises the whole input so the row shows something rather than empty.
func TestParseEvent_NoTitleUnknownToolFallsBackToStringifyJSON(t *testing.T) {
	line := `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","tool":"custom_tool","state":{"status":"completed","input":{"foo":"bar","count":3},"output":"ok"}}}`
	got, _ := parseEvent(line)
	res := got[0]
	if !strings.Contains(res.toolInput, `"foo":"bar"`) {
		t.Errorf("toolInput = %q, want stringifyJSON of the whole input", res.toolInput)
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

// TestParseEvent_ErrorStructuredObject verifies the 1.18+ structured error
// shape — `error: {name, data:{message, statusCode,...}}` — is decoded so
// the human message and HTTP status land in the event text instead of being
// dropped (which previously produced "error error").
func TestParseEvent_ErrorStructuredObject(t *testing.T) {
	// Shape mirrors a real 403 from opencode 1.18 (responseHeaders/body
	// trimmed; only name + data.message + data.statusCode matter here).
	line := `{"type":"error","timestamp":1785045798884,"sessionID":"ses_x","error":{"name":"APIError","data":{"message":"model not allowed for this key","statusCode":403,"isRetryable":false}}}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].kind != EventError {
		t.Fatalf("want 1 EventError, got %+v", got)
	}
	if !strings.Contains(got[0].text, "model not allowed for this key") {
		t.Errorf("text = %q, want contains 'model not allowed for this key'", got[0].text)
	}
	if !strings.Contains(got[0].text, "403") {
		t.Errorf("text = %q, want contains the HTTP status '403'", got[0].text)
	}
}

// TestParseEvent_ErrorStructuredNoStatusCode verifies a structured error
// without statusCode surfaces data.message alone (no trailing "HTTP 0").
func TestParseEvent_ErrorStructuredNoStatusCode(t *testing.T) {
	line := `{"type":"error","sessionID":"s1","error":{"name":"ConfigError","data":{"message":"missing provider key"}}}`
	got, _ := parseEvent(line)
	if !strings.Contains(got[0].text, "missing provider key") {
		t.Errorf("text = %q", got[0].text)
	}
	if strings.Contains(got[0].text, "HTTP") {
		t.Errorf("text should omit HTTP suffix when statusCode absent: %q", got[0].text)
	}
}

// TestParseEvent_ErrorStructuredNameOnly verifies that when data.message is
// absent but error.name is, the name itself becomes the message (better than
// the prior "error error" fallback).
func TestParseEvent_ErrorStructuredNameOnly(t *testing.T) {
	line := `{"type":"error","sessionID":"s1","error":{"name":"AbortError","data":{}}}`
	got, _ := parseEvent(line)
	if got[0].text != "AbortError" {
		t.Errorf("text = %q, want AbortError", got[0].text)
	}
}

// TestParseEvent_ErrorEmptyFieldsFallback verifies that when both message
// and the error object lack anything usable, the parser still falls back to
// the typed "<type> error" form.
func TestParseEvent_ErrorEmptyFieldsFallback(t *testing.T) {
	line := `{"type":"error","sessionID":"s1"}`
	got, _ := parseEvent(line)
	if got[0].text != "error error" {
		t.Errorf("text = %q, want 'error error' fallback", got[0].text)
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
