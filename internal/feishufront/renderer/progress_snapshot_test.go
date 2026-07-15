package renderer

import "testing"

// TestProgressStateCloneIsDeepCopy verifies Clone produces an independent copy:
// mutating the original (appending a tool, folding a repeat, appending text)
// must not change the clone. This guards the dispatcher's lock-free render
// path, which renders on a clone while the original may be mutated under the
// lock — a shallow copy would race and produce inconsistent cards.
func TestProgressStateCloneIsDeepCopy(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/a", false, "")
	s.AddText("hello")

	cp := s.Clone()

	// Mutate the original after cloning: a new tool, a repeat that folds into
	// the existing row (count++), and more text.
	s.AddToolUse("write", "/b", false, "")
	s.AddToolUse("read", "/a", false, "")
	s.AddText(" more")

	if len(cp.tools) != 1 || cp.tools[0].count != 1 {
		t.Errorf("clone tools = %+v, want exactly one read row with count 1 (deep copy)", cp.tools)
	}
	if cp.textBuf.String() != "hello" {
		t.Errorf("clone text = %q, want %q (textBuf must be copied)", cp.textBuf.String(), "hello")
	}
	if len(s.tools) != 2 || s.tools[0].count != 2 {
		t.Errorf("original tools = %+v, want read(count 2) + write", s.tools)
	}
	if s.textBuf.String() != "hello more" {
		t.Errorf("original text = %q, want %q", s.textBuf.String(), "hello more")
	}
}
