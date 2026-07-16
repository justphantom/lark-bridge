package claudebridge

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWrapThinkingFilter_DropsThinkingTokensLines verifies the claude-specific
// archive filter drops thinking_tokens lines (the bulk of a claude stream by
// volume) while keeping every other line kind. The generic sink/prune logic is
// covered in internal/streamarchive; this test isolates the filter.
func TestWrapThinkingFilter_DropsThinkingTokensLines(t *testing.T) {
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "cap.jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	wrapped := wrapThinkingFilter(f)

	lines := []string{
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":1024,"session_id":"s1"}`,
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-5"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":2048,"session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"system","subtype":"task_updated","task_id":"t1"}`,
	}
	for _, line := range lines {
		if _, err := wrapped.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	got, err := os.ReadFile(filepath.Join(dir, "cap.jsonl"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(got)
	if strings.Contains(body, "thinking_tokens") {
		t.Errorf("thinking_tokens lines must be dropped, got: %s", body)
	}
	for _, keep := range []string{`"subtype":"init"`, `"type":"assistant"`, "task_updated"} {
		if !strings.Contains(body, keep) {
			t.Errorf("non-thinking line %q must survive the filter, got: %s", keep, body)
		}
	}
	// 3 surviving lines (init, assistant, task_updated) → 3 newlines.
	if c := bytes.Count(got, []byte("\n")); c != 3 {
		t.Errorf("expected 3 surviving lines, got %d", c)
	}
}

// TestWrapThinkingFilter_NilPassthrough verifies a nil sink (archiving
// disabled) stays nil so callers can chain it unconditionally.
func TestWrapThinkingFilter_NilPassthrough(t *testing.T) {
	if got := wrapThinkingFilter(nil); got != nil {
		t.Error("wrapThinkingFilter(nil) must return nil")
	}
}
