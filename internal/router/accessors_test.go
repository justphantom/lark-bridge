package router

import (
	"testing"

	"github.com/justphantom/lark-bridge/internal/log"
)

// newTestRouter builds an in-memory router (no persistence) pre-loaded with a
// binding for chatID, so Set* accessors have a target to mutate.
func newTestRouter(t *testing.T, chatID string) *Router {
	t.Helper()
	r, err := New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	r.bindings[chatID] = Binding{}
	return r
}

// TestSetModelSpec_WritesAndPersists verifies SetModelSpec updates the field
// and leaves others untouched.
func TestSetModelSpec_WritesAndPersists(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetModelSpec("c1", "sonnet")
	b, _ := r.Lookup("c1")
	if b.ModelSpec != "sonnet" {
		t.Errorf("ModelSpec = %q, want sonnet", b.ModelSpec)
	}
}

// TestSetAgent verifies the agent field round-trips.
func TestSetAgent(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetAgent("c1", "build")
	b, _ := r.Lookup("c1")
	if b.Agent != "build" {
		t.Errorf("Agent = %q, want build", b.Agent)
	}
}

// TestSetSessionID verifies session id write + that a second write to the same
// value is a no-op (mutate returns false, no log spam).
func TestSetSessionID(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetSessionID("c1", "sess-1")
	b, _ := r.Lookup("c1")
	if b.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", b.SessionID)
	}
	// Overwrite with a different value.
	r.SetSessionID("c1", "sess-2")
	b, _ = r.Lookup("c1")
	if b.SessionID != "sess-2" {
		t.Errorf("SessionID = %q, want sess-2", b.SessionID)
	}
}

// TestSetDirectory verifies the directory field.
func TestSetDirectory(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetDirectory("c1", "/work")
	b, _ := r.Lookup("c1")
	if b.Directory != "/work" {
		t.Errorf("Directory = %q, want /work", b.Directory)
	}
}

// TestSetPermissionMode verifies the Claude permission mode field.
func TestSetPermissionMode(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetPermissionMode("c1", "plan")
	b, _ := r.Lookup("c1")
	if b.PermissionMode != "plan" {
		t.Errorf("PermissionMode = %q, want plan", b.PermissionMode)
	}
}

// TestSetEffortLevel verifies the Claude effort level field.
func TestSetEffortLevel(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetEffortLevel("c1", "max")
	b, _ := r.Lookup("c1")
	if b.EffortLevel != "max" {
		t.Errorf("EffortLevel = %q, want max", b.EffortLevel)
	}
}

// TestSetSettingsFile verifies the settings file field.
func TestSetSettingsFile(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetSettingsFile("c1", "/home/u/.claude/k.json")
	b, _ := r.Lookup("c1")
	if b.SettingsFile != "/home/u/.claude/k.json" {
		t.Errorf("SettingsFile = %q", b.SettingsFile)
	}
}

// TestSetMethods_LeaveOtherFieldsUntouched verifies each Set* mutates only its
// own field — a regression here would silently corrupt the binding.
func TestSetMethods_LeaveOtherFieldsUntouched(t *testing.T) {
	r := newTestRouter(t, "c1")
	// Seed every field.
	r.SetModelSpec("c1", "sonnet")
	r.SetAgent("c1", "build")
	r.SetSessionID("c1", "sess-1")
	r.SetDirectory("c1", "/work")
	r.SetPermissionMode("c1", "plan")
	r.SetEffortLevel("c1", "max")
	r.SetSettingsFile("c1", "/k.json")

	// Now change only ModelSpec; everything else must stay.
	r.SetModelSpec("c1", "opus")
	b, _ := r.Lookup("c1")
	if b.ModelSpec != "opus" {
		t.Errorf("ModelSpec = %q, want opus", b.ModelSpec)
	}
	if b.Agent != "build" || b.SessionID != "sess-1" || b.Directory != "/work" ||
		b.PermissionMode != "plan" || b.EffortLevel != "max" || b.SettingsFile != "/k.json" {
		t.Errorf("SetModelSpec corrupted other fields: %+v", b)
	}
}

// TestSetMethods_NoOpOnMissingBinding verifies Set* is a no-op when the binding
// does not exist (does not panic, does not create a binding).
func TestSetMethods_NoOpOnMissingBinding(t *testing.T) {
	r, err := New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	// None of these should panic.
	r.SetModelSpec("ghost", "x")
	r.SetAgent("ghost", "x")
	r.SetSessionID("ghost", "x")
	r.SetDirectory("ghost", "x")
	r.SetPermissionMode("ghost", "x")
	r.SetEffortLevel("ghost", "x")
	r.SetSettingsFile("ghost", "x")
	if _, ok := r.Lookup("ghost"); ok {
		t.Fatal("Set* on missing binding must not create one")
	}
}

// TestAllBindings_IsSnapshot verifies the returned map is a copy: mutating it
// does not affect the router's internal state.
func TestAllBindings_IsSnapshot(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetModelSpec("c1", "sonnet")
	snap := r.AllBindings()
	snap["c1"] = Binding{ModelSpec: "tampered"}
	// Internal state must be unaffected.
	b, _ := r.Lookup("c1")
	if b.ModelSpec != "sonnet" {
		t.Errorf("AllBindings snapshot leaked mutation: ModelSpec = %q, want sonnet", b.ModelSpec)
	}
}

// TestTitleOf verifies TitleOf returns the title or empty string.
func TestTitleOf(t *testing.T) {
	r, err := New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	if title := r.TitleOf("absent"); title != "" {
		t.Errorf("TitleOf(absent) = %q, want empty", title)
	}
	r.bindings["c1"] = Binding{Title: "my chat"}
	if title := r.TitleOf("c1"); title != "my chat" {
		t.Errorf("TitleOf = %q, want 'my chat'", title)
	}
}
