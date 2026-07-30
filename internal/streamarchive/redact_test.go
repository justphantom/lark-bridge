package streamarchive

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactingWriter_SensitiveFieldRedacted(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `{"prompt":"my secret prompt","text":"some text","content":"sensitive content"}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "my secret prompt") {
		t.Errorf("expected prompt to be redacted, got: %s", out)
	}
	if strings.Contains(out, "some text") {
		t.Errorf("expected text to be redacted, got: %s", out)
	}
	if strings.Contains(out, "sensitive content") {
		t.Errorf("expected content to be redacted, got: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output, got: %s", out)
	}
}

func TestRedactingWriter_NonSensitiveFieldPreserved(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `{"model":"claude-3-opus-20240229","role":"assistant","stop_reason":"end_turn"}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "claude-3-opus-20240229") {
		t.Errorf("expected model to be preserved, got: %s", out)
	}
	if !strings.Contains(out, "assistant") {
		t.Errorf("expected role to be preserved, got: %s", out)
	}
	if strings.Contains(out, "[REDACTED]") {
		t.Errorf("unexpected [REDACTED] in output: %s", out)
	}
}

func TestRedactingWriter_NestedObjectRedaction(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	// input is a sensitive field, so the entire nested object is redacted.
	input := `{"type":"tool_use","name":"Read","input":{"file_path":"/etc/passwd","text":"root:x:0:0:root:/root:/bin/bash"}}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "root:x:0:0") {
		t.Errorf("expected nested text to be redacted, got: %s", out)
	}
	if !strings.Contains(out, "Read") {
		t.Errorf("expected tool name to be preserved, got: %s", out)
	}
	if strings.Contains(out, "file_path") {
		t.Errorf("expected file_path to be inside a redacted input field, but got: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output, got: %s", out)
	}
}

func TestRedactingWriter_RecursiveNestedObjectRedaction(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	// A non-sensitive field contains a nested object with sensitive keys,
	// testing redactMap's recursion into nested objects.
	input := `{"type":"tool_result","metadata":{"prompt":"secret query","other":"keep this"}}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "secret query") {
		t.Errorf("expected nested prompt to be redacted, got: %s", out)
	}
	if !strings.Contains(out, "keep this") {
		t.Errorf("expected non-sensitive nested field to be preserved, got: %s", out)
	}
	if !strings.Contains(out, "type") {
		t.Errorf("expected top-level type to be preserved, got: %s", out)
	}
	if !strings.Contains(out, "metadata") {
		t.Errorf("expected metadata key to be preserved, got: %s", out)
	}
}

func TestRedactingWriter_ArrayElementRedaction(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `{"content":[{"type":"text","text":"secret reply"},{"type":"text","text":"another secret"}]}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "secret reply") {
		t.Errorf("expected array element text to be redacted, got: %s", out)
	}
	if strings.Contains(out, "another secret") {
		t.Errorf("expected array element text to be redacted, got: %s", out)
	}
}

func TestRedactingWriter_UnparseableLinePassesVerbatim(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `this is not json at all`
	n, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(input) {
		t.Fatalf("Write returned %d, want %d", n, len(input))
	}

	out := buf.String()
	if out != input {
		t.Errorf("expected verbatim output, got: %q", out)
	}
}

func TestRedactingWriter_EmptyLinePassesThrough(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `` // empty
	n, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != 0 {
		t.Fatalf("Write returned %d, want 0", n)
	}

	out := buf.String()
	if out != "" {
		t.Errorf("expected empty output, got: %q", out)
	}
}

func TestRedactingWriter_InputOutputRedacted(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `{"type":"tool_result","tool_use_id":"tu_01","content":"Here is the file content","input":"some sensitive input"}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "Here is the file content") {
		t.Errorf("expected content to be redacted, got: %s", out)
	}
	if strings.Contains(out, "some sensitive input") {
		t.Errorf("expected input to be redacted, got: %s", out)
	}
	if !strings.Contains(out, "tool_use_id") {
		t.Errorf("expected tool_use_id to be preserved, got: %s", out)
	}
}

func TestRedactingWriter_ThinkingRedacted(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `{"type":"thinking","thinking":"The model's internal reasoning","text":"visible text"}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "The model's internal reasoning") {
		t.Errorf("expected thinking to be redacted, got: %s", out)
	}
	if strings.Contains(out, "visible text") {
		t.Errorf("expected text to be redacted, got: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output, got: %s", out)
	}
}

func TestRedactingWriter_FileTextRedacted(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `{"type":"tool_result","name":"Read","file_text":"SSH private key content"}`
	_, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, "SSH private key content") {
		t.Errorf("expected file_text to be redacted, got: %s", out)
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("expected [REDACTED] in output, got: %s", out)
	}
}

// TestRedactingWriter_NewlinePreserved guards the NDJSON framing: the pump
// writes "line\n" per record; if the trailing newline were dropped the
// archive would collapse into one giant line.
func TestRedactingWriter_NewlinePreserved(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	input := `{"prompt":"secret","type":"user"}` + "\n"
	n, err := rw.Write([]byte(input))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != len(input) {
		t.Errorf("Write returned %d, want %d (io.Writer contract counts bytes consumed from p)", n, len(input))
	}

	out := buf.String()
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("expected output to end with newline, got: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("expected exactly one newline, got: %q", out)
	}
}

// TestRedactingWriter_MultiLineFraming writes two NDJSON records and checks
// both remain separately framed after redaction.
func TestRedactingWriter_MultiLineFraming(t *testing.T) {
	var buf bytes.Buffer
	rw := NewRedactingWriter(&buf)

	for _, line := range []string{
		`{"prompt":"one"}` + "\n",
		`{"text":"two"}` + "\n",
	} {
		if _, err := rw.Write([]byte(line)); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 NDJSON lines, got %d: %q", len(lines), buf.String())
	}
	for _, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Errorf("line not valid JSON: %q: %v", l, err)
		}
	}
}
