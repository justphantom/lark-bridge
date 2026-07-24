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

// TestProgressStateClone_TodosDeepCopy guards the lock-free render path against
// a todos data race: after Clone, mutating the original's todos (the backend
// resends the whole list each update, so AddTodo reuses the backing array via
// append-to-zero) must not change the clone's snapshot.
func TestProgressStateClone_TodosDeepCopy(t *testing.T) {
	s := NewProgressState()
	s.AddTodo([]TodoItem{{Content: "a", Status: "pending"}})

	cp := s.Clone()

	// Overwrite the original's todos in place (AddTodo reuses s.todos[:0]).
	s.AddTodo([]TodoItem{{Content: "b", Status: "completed"}, {Content: "c", Status: "cancelled"}})

	if len(cp.todos) != 1 || cp.todos[0].Content != "a" {
		t.Errorf("clone todos = %+v, want [a] (deep copy, untouched by later overwrite)", cp.todos)
	}
	if len(s.todos) != 2 || s.todos[0].Content != "b" {
		t.Errorf("original todos = %+v, want [b,c] after overwrite", s.todos)
	}
}
