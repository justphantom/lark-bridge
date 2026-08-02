package router

import (
	"path/filepath"
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

// TestSetMode verifies the miniagent -mode field round-trips.
func TestSetMode(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetMode("c1", "auto")
	b, _ := r.Lookup("c1")
	if b.Mode != "auto" {
		t.Errorf("Mode = %q, want auto", b.Mode)
	}
}

// TestSetThinking verifies the miniagent -thinking field round-trips.
func TestSetThinking(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetThinking("c1", "high")
	b, _ := r.Lookup("c1")
	if b.Thinking != "high" {
		t.Errorf("Thinking = %q, want high", b.Thinking)
	}
}

// TestSetMaxIterations verifies the miniagent -max-iterations field round-trips
// (int, unlike the string miniagent pins). 0 is the clear value: it means "do
// not pass the flag", matching buildArgs's >0 gate.
func TestSetMaxIterations(t *testing.T) {
	r := newTestRouter(t, "c1")
	r.SetMaxIterations("c1", 50)
	b, _ := r.Lookup("c1")
	if b.MaxIterations != 50 {
		t.Errorf("MaxIterations = %d, want 50", b.MaxIterations)
	}
	// 0 clears the pin.
	r.SetMaxIterations("c1", 0)
	b, _ = r.Lookup("c1")
	if b.MaxIterations != 0 {
		t.Errorf("after clear: MaxIterations = %d, want 0", b.MaxIterations)
	}
}

// TestSetMode_PersistsAcrossReload verifies SetMode writes through to disk:
// after Close, a fresh Router loading the same persistPath must surface the
// pinned mode. Guards the saveAsync coalescer end-to-end for the new field.
func TestSetMode_PersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.json")
	r1, err := New(path, log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	r1.Bind("c1", "", "", "", "", "")
	r1.SetMode("c1", "auto")
	r1.SetThinking("c1", "max")
	r1.SetMaxIterations("c1", 42)
	r1.Close()

	r2, err := New(path, log.Nop())
	if err != nil {
		t.Fatalf("router reload: %v", err)
	}
	defer r2.Close()
	b, ok := r2.Lookup("c1")
	if !ok {
		t.Fatal("binding missing after reload")
	}
	if b.Mode != "auto" {
		t.Errorf("Mode after reload = %q, want auto", b.Mode)
	}
	if b.Thinking != "max" {
		t.Errorf("Thinking after reload = %q, want max", b.Thinking)
	}
	if b.MaxIterations != 42 {
		t.Errorf("MaxIterations after reload = %d, want 42", b.MaxIterations)
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
	r.SetMode("c1", "auto")
	r.SetThinking("c1", "high")
	r.SetMaxIterations("c1", 30)

	// Now change only ModelSpec; everything else must stay.
	r.SetModelSpec("c1", "opus")
	b, _ := r.Lookup("c1")
	if b.ModelSpec != "opus" {
		t.Errorf("ModelSpec = %q, want opus", b.ModelSpec)
	}
	if b.Agent != "build" || b.SessionID != "sess-1" || b.Directory != "/work" ||
		b.PermissionMode != "plan" || b.EffortLevel != "max" || b.SettingsFile != "/k.json" ||
		b.Mode != "auto" || b.Thinking != "high" || b.MaxIterations != 30 {
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
	r.SetMode("ghost", "x")
	r.SetThinking("ghost", "x")
	r.SetMaxIterations("ghost", 5)
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
