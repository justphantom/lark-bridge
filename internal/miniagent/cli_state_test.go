package miniagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// findMiniagentBinary locates the miniagent CLI binary for integration tests.
// Looks for the local-dev build (../../miniagent/miniagent relative to this
// package) and falls back to /usr/local/bin. Skips the suite if neither is
// executable — these tests need a real CLI built from the same source tree.
func findMiniagentBinary(t *testing.T) string {
	t.Helper()
	candidates := []string{
		filepath.Join("..", "..", "..", "miniagent", "miniagent"),
		"/usr/local/bin/miniagent",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			if path, err := exec.LookPath(c); err == nil {
				_ = path
				return c
			}
		}
	}
	t.Skip("miniagent binary not found; build ../miniagent first or install to /usr/local/bin")
	return ""
}

// newCLIState builds a CLIState rooted at a fresh temp dir. Every test gets
// its own state-dir so they do not interfere.
func newCLIState(t *testing.T) *CLIState {
	t.Helper()
	bin := findMiniagentBinary(t)
	dir := t.TempDir()
	return NewCLIState(bin, dir, "test-key", "http://localhost:8080")
}

// TestCLIState_ShowCurrent_Empty verifies a fresh state reports all empty
// pins (no session, no model/dir/permission). This is the base case for
// activeTurnConfig's fallback-to-global path.
func TestCLIState_ShowCurrent_Empty(t *testing.T) {
	c := newCLIState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := c.ShowCurrent(ctx, "chat-1")
	if err != nil {
		t.Fatalf("ShowCurrent: %v", err)
	}
	if state.ChatID != "chat-1" {
		t.Errorf("ChatID = %q, want chat-1", state.ChatID)
	}
	if state.SessionID != "" || state.Model != "" || state.Directory != "" || state.Permission != "" {
		t.Errorf("expected all-empty pins, got %+v", state)
	}
}

// TestCLIState_SetModel_Readback pins a model via SetModel, then verifies
// ShowCurrent returns it. Empty-string SetModel must clear the pin.
func TestCLIState_SetModel_Readback(t *testing.T) {
	c := newCLIState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.SetModel(ctx, "c1", "gpt-4o"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	state, _ := c.ShowCurrent(ctx, "c1")
	if state.Model != "gpt-4o" {
		t.Errorf("after SetModel: Model = %q, want gpt-4o", state.Model)
	}
	// Clear.
	if err := c.SetModel(ctx, "c1", ""); err != nil {
		t.Fatalf("SetModel clear: %v", err)
	}
	state, _ = c.ShowCurrent(ctx, "c1")
	if state.Model != "" {
		t.Errorf("after clear: Model = %q, want empty", state.Model)
	}
}

// TestCLIState_SetDir_Readback pins and clears a working directory.
func TestCLIState_SetDir_Readback(t *testing.T) {
	c := newCLIState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.SetDir(ctx, "c1", "/tmp"); err != nil {
		t.Fatalf("SetDir: %v", err)
	}
	state, _ := c.ShowCurrent(ctx, "c1")
	if state.Directory != "/tmp" {
		t.Errorf("after SetDir: Directory = %q, want /tmp", state.Directory)
	}
	if err := c.SetDir(ctx, "c1", ""); err != nil {
		t.Fatalf("SetDir clear: %v", err)
	}
	state, _ = c.ShowCurrent(ctx, "c1")
	if state.Directory != "" {
		t.Errorf("after clear: Directory = %q, want empty", state.Directory)
	}
}

// TestCLIState_SetPermission_Readback pins and clears the permission mode.
func TestCLIState_SetPermission_Readback(t *testing.T) {
	c := newCLIState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.SetPermission(ctx, "c1", "free"); err != nil {
		t.Fatalf("SetPermission: %v", err)
	}
	state, _ := c.ShowCurrent(ctx, "c1")
	if state.Permission != "free" {
		t.Errorf("after SetPermission: Permission = %q, want free", state.Permission)
	}
	if err := c.SetPermission(ctx, "c1", ""); err != nil {
		t.Fatalf("SetPermission clear: %v", err)
	}
	state, _ = c.ShowCurrent(ctx, "c1")
	if state.Permission != "" {
		t.Errorf("after clear: Permission = %q, want empty", state.Permission)
	}
}

// TestCLIState_NewSession_Readback creates a session and verifies it shows
// up in ShowCurrent.SessionID and ListSessions (marked current).
func TestCLIState_NewSession_Readback(t *testing.T) {
	c := newCLIState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sid, err := c.NewSession(ctx, "c1")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sid == "" {
		t.Fatal("NewSession returned empty session id")
	}
	state, _ := c.ShowCurrent(ctx, "c1")
	if state.SessionID != sid {
		t.Errorf("current SessionID = %q, want %q", state.SessionID, sid)
	}
	sessions, err := c.ListSessions(ctx, "c1")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("ListSessions = %d entries, want 1", len(sessions))
	}
	if sessions[0].ID != sid || !sessions[0].Current {
		t.Errorf("ListSessions[0] = %+v, want ID=%s Current=true", sessions[0], sid)
	}
}

// TestCLIState_DeleteSession verifies -del-session removes the session and
// that DeleteSession("") deletes the current one (bridge convenience).
func TestCLIState_DeleteSession(t *testing.T) {
	c := newCLIState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sid, _ := c.NewSession(ctx, "c1")
	if err := c.DeleteSession(ctx, "c1", sid); err != nil {
		t.Fatalf("DeleteSession by id: %v", err)
	}
	sessions, _ := c.ListSessions(ctx, "c1")
	if len(sessions) != 0 {
		t.Errorf("after delete: %d sessions, want 0", len(sessions))
	}
	// Create another, then delete by empty (current).
	sid2, _ := c.NewSession(ctx, "c1")
	if err := c.DeleteSession(ctx, "c1", ""); err != nil {
		t.Fatalf("DeleteSession current: %v", err)
	}
	sessions, _ = c.ListSessions(ctx, "c1")
	if len(sessions) != 0 {
		t.Errorf("after delete-current: %d sessions, want 0", len(sessions))
	}
	_ = sid2
}

// TestCLIState_MemoryCRUD exercises the full memory lifecycle via the CLI:
// list-empty → seed → list → search → delete → list-empty. Seeds by writing
// the JSON state file directly (the CLI's memory_set is an LLM tool; no
// -set-memory subcommand exists). This mirrors how the CLI writes facts.
func TestCLIState_MemoryCRUD(t *testing.T) {
	c := newCLIState(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Empty list returns a non-nil empty slice.
	facts, err := c.ListFacts(ctx, "c1", "")
	if err != nil {
		t.Fatalf("ListFacts empty: %v", err)
	}
	if facts == nil || len(facts) != 0 {
		t.Errorf("empty ListFacts = %v, want empty non-nil", facts)
	}

	// Seed two facts via the on-disk format the CLI itself writes.
	seedFacts(t, c.stateDir, "c1", map[string]string{
		"user.lang":  "中文",
		"project.id": "lark-bridge",
	})

	// List returns both, sorted by key.
	facts, err = c.ListFacts(ctx, "c1", "")
	if err != nil {
		t.Fatalf("ListFacts: %v", err)
	}
	if len(facts) != 2 {
		t.Fatalf("ListFacts = %d entries, want 2", len(facts))
	}
	if facts[0].Key != "project.id" || facts[1].Key != "user.lang" {
		t.Errorf("sort order: %+v", facts)
	}

	// Prefix filter narrows to one.
	facts, _ = c.ListFacts(ctx, "c1", "user.")
	if len(facts) != 1 || facts[0].Key != "user.lang" {
		t.Errorf("prefix user. = %+v, want user.lang only", facts)
	}

	// Search hits value.
	facts, _ = c.SearchFacts(ctx, "c1", "lark")
	if len(facts) != 1 || facts[0].Key != "project.id" {
		t.Errorf("search lark = %+v, want project.id", facts)
	}

	// Delete existing key.
	existed, err := c.DeleteFact(ctx, "c1", "user.lang")
	if err != nil {
		t.Fatalf("DeleteFact: %v", err)
	}
	if !existed {
		t.Error("DeleteFact returned existed=false for present key")
	}
	facts, _ = c.ListFacts(ctx, "c1", "user.")
	if len(facts) != 0 {
		t.Errorf("after delete: %d user. facts, want 0", len(facts))
	}

	// Delete missing key returns existed=false, not an error.
	existed, err = c.DeleteFact(ctx, "c1", "user.lang")
	if err != nil {
		t.Fatalf("DeleteFact missing: %v", err)
	}
	if existed {
		t.Error("DeleteFact returned existed=true for absent key")
	}
}

// seedFacts writes a fact file in the CLI's on-disk format. Used only by
// tests; production writes happen via the memory_set tool inside the CLI.
func seedFacts(t *testing.T, stateDir, chatID string, kv map[string]string) {
	t.Helper()
	path := filepath.Join(stateDir, "miniagent", "memory", sanitizeChatID(chatID)+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	var sb strings.Builder
	sb.WriteString("{")
	first := true
	for k, v := range kv {
		if !first {
			sb.WriteString(",")
		}
		first = false
		sb.WriteString(`"` + k + `":{"key":"` + k + `","value":"` + v + `","source":"test","updated_at":"2026-01-01T00:00:00Z"}`)
	}
	sb.WriteString("}")
	if err := os.WriteFile(path, []byte(sb.String()), 0o600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
}
