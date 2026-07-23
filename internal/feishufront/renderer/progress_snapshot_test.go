package renderer

import "testing"

// TestProgressStateCloneIsDeepCopy verifies Clone produces an independent copy:
// mutating the original (appending a tool, folding a repeat) must not change
// the clone. This guards the dispatcher's lock-free render path, which renders
// on a clone while the original may be mutated under the lock — a shallow copy
// would race and produce inconsistent cards.
func TestProgressStateCloneIsDeepCopy(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("read", "/a", false, "")

	cp := s.Clone()

	// Mutate the original after cloning: a new tool and a repeat that folds
	// into the existing row (count++).
	s.AddToolUse("write", "/b", false, "")
	s.AddToolUse("read", "/a", false, "")

	if len(cp.tools) != 1 || cp.tools[0].count != 1 {
		t.Errorf("clone tools = %+v, want exactly one read row with count 1 (deep copy)", cp.tools)
	}
	if len(s.tools) != 2 || s.tools[0].count != 2 {
		t.Errorf("original tools = %+v, want read(count 2) + write", s.tools)
	}
}
