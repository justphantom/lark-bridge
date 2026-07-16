package goose

import (
	"strings"
	"testing"
)

// parseLine must map each goose content block type to the right Event kind,
// and treat the complete line as terminal. These cases are the parser's
// requirements: each documents one wire shape observed from goose 1.43.0.
func TestParseLine(t *testing.T) {
	var b strings.Builder

	// thinking chunk
	evs, term, err := parseLine([]byte(`{"type":"message","message":{"content":[{"type":"thinking","thinking":"hmm"}]}}`), &b)
	if err != nil || term || len(evs) != 1 || evs[0].GetType() != EventThinking || evs[0].GetText() != "hmm" {
		t.Fatalf("thinking parse: evs=%v term=%v err=%v", evs, term, err)
	}

	// text chunk: accumulated into textAcc
	evs, term, err = parseLine([]byte(`{"type":"message","message":{"content":[{"type":"text","text":"hello"}]}}`), &b)
	if err != nil || term || len(evs) != 1 || evs[0].GetType() != EventText || evs[0].GetText() != "hello" {
		t.Fatalf("text parse: evs=%v term=%v err=%v", evs, term, err)
	}
	if b.String() != "hello" {
		t.Fatalf("textAcc = %q, want %q", b.String(), "hello")
	}

	// toolRequest → tool_use with name from toolCall.value.name
	evs, term, err = parseLine([]byte(`{"type":"message","message":{"content":[{"type":"toolRequest","id":"tool_1","toolCall":{"status":"success","value":{"name":"shell","arguments":{"command":"ls"}}}}]}}`), &b)
	if err != nil || term || len(evs) != 1 || evs[0].GetType() != EventToolUse || evs[0].GetToolName() != "shell" || evs[0].GetToolID() != "tool_1" {
		t.Fatalf("toolRequest parse: evs=%v term=%v err=%v", evs, term, err)
	}

	// toolResponse success → tool_result, isError=false, text from content[].text
	evs, term, err = parseLine([]byte(`{"type":"message","message":{"content":[{"type":"toolResponse","id":"tool_1","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"done"}],"isError":false}}}]}}`), &b)
	if err != nil || term || len(evs) != 1 || evs[0].GetType() != EventToolResult || evs[0].GetText() != "done" || evs[0].GetIsToolError() {
		t.Fatalf("toolResponse ok parse: evs=%v term=%v err=%v", evs, term, err)
	}

	// toolResponse failure → isToolError=true (structural, not prefix sniff)
	evs, term, err = parseLine([]byte(`{"type":"message","message":{"content":[{"type":"toolResponse","id":"tool_2","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"exit 1"}],"isError":true}}}]}}`), &b)
	if err != nil || len(evs) != 1 || !evs[0].GetIsToolError() {
		t.Fatalf("toolResponse err parse: evs=%v err=%v", evs, err)
	}

	// complete → terminal, carries usage
	evs, term, err = parseLine([]byte(`{"type":"complete","total_tokens":4240,"input_tokens":4197,"output_tokens":43}`), &b)
	if err != nil || !term || len(evs) != 1 || evs[0].GetType() != EventComplete || evs[0].GetInputTokens() != 4197 || evs[0].GetOutputTokens() != 43 {
		t.Fatalf("complete parse: evs=%v term=%v err=%v", evs, term, err)
	}

	// unknown top-level type: no events, no error (forward-compat)
	evs, term, err = parseLine([]byte(`{"type":"future_event","data":"x"}`), &b)
	if err != nil || term || len(evs) != 0 {
		t.Fatalf("unknown type: evs=%v term=%v err=%v", evs, term, err)
	}

	// blank line: no-op
	evs, term, err = parseLine([]byte(`   `), &b)
	if err != nil || term || len(evs) != 0 {
		t.Fatalf("blank line: evs=%v term=%v err=%v", evs, term, err)
	}
}

// TestParseLine_MalformedJSON returns an error so the pump can log and skip.
func TestParseLine_MalformedJSON(t *testing.T) {
	var b strings.Builder
	_, _, err := parseLine([]byte(`{not json`), &b)
	if err == nil {
		t.Fatal("malformed JSON want error, got nil")
	}
}

// TestParseContent_UnknownBlock is skipped (ok=false) so future content types
// do not crash the parser.
func TestParseContent_UnknownBlock(t *testing.T) {
	var b strings.Builder
	evs, term, err := parseLine([]byte(`{"type":"message","message":{"content":[{"type":"new_block","x":1}]}}`), &b)
	if err != nil || term || len(evs) != 0 {
		t.Fatalf("unknown content block: evs=%v term=%v err=%v", evs, term, err)
	}
}

// TestParseToolResponse_MultiContentConcat verifies multiple content[].text
// entries are concatenated with newlines (goose usually emits one, but the
// slice is defended).
func TestParseToolResponse_MultiContentConcat(t *testing.T) {
	var b strings.Builder
	evs, _, err := parseLine([]byte(`{"type":"message","message":{"content":[{"type":"toolResponse","id":"t1","toolResult":{"status":"success","value":{"content":[{"type":"text","text":"line1"},{"type":"text","text":"line2"}]}}}]}}`), &b)
	if err != nil || len(evs) != 1 || evs[0].GetText() != "line1\nline2" {
		t.Fatalf("multi-content: evs=%v err=%v", evs, err)
	}
}
