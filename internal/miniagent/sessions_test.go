package miniagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSessions_ImplicitCreateOnFirstAppend verifies the first Append for a
// chat lazily creates a session pointer, so a plain prompt needs no explicit
// /session-new first.
func TestSessions_ImplicitCreateOnFirstAppend(t *testing.T) {
	h := newTestHistory(t)
	if sid := h.Current("c"); sid != "" {
		t.Fatalf("Current before any turn = %q, want empty", sid)
	}
	h.Append("c", []Message{{Role: "user", Content: "hi"}})
	sid := h.Current("c")
	if sid == "" {
		t.Fatal("Current after first Append = empty, want an auto-created session id")
	}
	list, err := h.ListSessions("c")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 1 || list[0].ID != sid || !list[0].Current {
		t.Errorf("List = %+v, want single current session %q", list, sid)
	}
}

// TestSessions_NewSessionKeepsOld verifies /session-new points at a fresh
// empty session while the prior one stays on disk and listable.
func TestSessions_NewSessionKeepsOld(t *testing.T) {
	h := newTestHistory(t)
	h.Append("c", []Message{{Role: "user", Content: "first"}})
	old := h.Current("c")

	s2, err := h.NewSession("c")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if s2 == old {
		t.Fatalf("NewSession returned the same id %q", s2)
	}
	if h.Current("c") != s2 {
		t.Errorf("Current = %q, want new %q", h.Current("c"), s2)
	}
	// New active session is empty; the old turn is NOT in it.
	if got := h.Load("c"); len(got) != 0 {
		t.Errorf("Load new session = %d msgs, want 0", len(got))
	}
	list, _ := h.ListSessions("c")
	if len(list) != 2 {
		t.Errorf("List = %d sessions, want 2 (old kept)", len(list))
	}
}

// TestSessions_NewSessionTwiceDistinct verifies two rapid NewSession calls do
// not collide on the same-second filename (the nanosecond disambiguator).
func TestSessions_NewSessionTwiceDistinct(t *testing.T) {
	h := newTestHistory(t)
	a, _ := h.NewSession("c")
	b, _ := h.NewSession("c")
	if a == b {
		t.Errorf("two NewSession ids equal: %q", a)
	}
	if !validSessionID(a) || !validSessionID(b) {
		t.Errorf("NewSession produced invalid id: %q / %q", a, b)
	}
}

// TestSessions_UseSessionResumes verifies the resume (接续) path: after
// switching branches, Load returns the selected branch's history.
func TestSessions_UseSessionResumes(t *testing.T) {
	h := newTestHistory(t)
	h.Append("c", []Message{{Role: "user", Content: "q1"}, {Role: "assistant", Content: "a1"}})
	s1 := h.Current("c")

	if _, err := h.NewSession("c"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	h.Append("c", []Message{{Role: "user", Content: "q2"}, {Role: "assistant", Content: "a2"}})
	// Active (s2) branch sees q2/a2 only.
	if got := h.Load("c"); len(got) != 2 || got[0].Content != "q2" {
		t.Errorf("Load s2 = %+v, want q2/a2", got)
	}

	// Switch back to s1 — its context must come back whole.
	if err := h.UseSession("c", s1); err != nil {
		t.Fatalf("UseSession: %v", err)
	}
	if got := h.Load("c"); len(got) != 2 || got[0].Content != "q1" {
		t.Errorf("Load after resume = %+v, want q1/a1", got)
	}
	if h.Current("c") != s1 {
		t.Errorf("Current = %q, want %q after resume", h.Current("c"), s1)
	}
}

// TestSessions_UseSessionErrors verifies bad resume targets are rejected
// rather than silently pointing the chat at a nonexistent file.
func TestSessions_UseSessionErrors(t *testing.T) {
	h := newTestHistory(t)
	h.Append("c", []Message{{Role: "user", Content: "x"}})

	if err := h.UseSession("c", "20200101-000000"); err == nil {
		t.Error("UseSession nonexistent id: want error, got nil")
	}
	if err := h.UseSession("c", "bad/id"); err == nil {
		t.Error("UseSession path-traversal id: want error, got nil")
	}
	if err := h.UseSession("c", "UPPERCASE"); err == nil {
		t.Error("UseSession invalid charset: want error, got nil")
	}
	// A failed UseSession must not have moved the pointer.
	if h.Current("c") == "20200101-000000" {
		t.Error("pointer moved despite UseSession error")
	}
}

// TestSessions_DeleteActiveClearsPointer verifies deleting the active session
// resets the chat so the next prompt starts fresh.
func TestSessions_DeleteActiveClearsPointer(t *testing.T) {
	h := newTestHistory(t)
	h.Append("c", []Message{{Role: "user", Content: "x"}})
	if err := h.DeleteSession("c", ""); err != nil {
		t.Fatalf("DeleteSession active: %v", err)
	}
	if h.Current("c") != "" {
		t.Errorf("Current after active delete = %q, want empty", h.Current("c"))
	}
	if got := h.Load("c"); got != nil {
		t.Errorf("Load after active delete = %+v, want nil", got)
	}
}

// TestSessions_DeleteById verifies a non-active session is removed without
// disturbing the active one.
func TestSessions_DeleteById(t *testing.T) {
	h := newTestHistory(t)
	h.Append("c", []Message{{Role: "user", Content: "a"}})
	old := h.Current("c")
	cur, _ := h.NewSession("c")

	if err := h.DeleteSession("c", old); err != nil {
		t.Fatalf("DeleteSession by id: %v", err)
	}
	if h.Current("c") != cur {
		t.Errorf("Current = %q, want unchanged %q", h.Current("c"), cur)
	}
	if err := h.UseSession("c", old); err == nil {
		t.Error("UseSession deleted id: want error, got nil")
	}
}

// TestSessions_DeleteWhenNone verifies deleting with nothing to delete is an
// error, not a silent success.
func TestSessions_DeleteWhenNone(t *testing.T) {
	h := newTestHistory(t)
	if err := h.DeleteSession("c", ""); err == nil {
		t.Error("DeleteSession active when none: want error, got nil")
	}
	if err := h.DeleteSession("c", "20200101-000000"); err == nil {
		t.Error("DeleteSession by id when none: want error, got nil")
	}
}

// TestSessions_ListOldestFirst verifies ListSessions orders by mtime ascending
// and marks exactly one current (the freshly materialized one).
func TestSessions_ListOldestFirst(t *testing.T) {
	h := newTestHistory(t)
	h.Append("c", []Message{{Role: "user", Content: "1"}})
	time.Sleep(50 * time.Millisecond) // distinct mtime from the new session file
	if _, err := h.NewSession("c"); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	list, _ := h.ListSessions("c")
	if len(list) != 2 {
		t.Fatalf("List = %d, want 2", len(list))
	}
	if !list[0].ModTime.Before(list[1].ModTime) {
		t.Errorf("not oldest-first: %+v", list)
	}
	cur := 0
	for _, s := range list {
		if s.Current {
			cur++
		}
	}
	if cur != 1 {
		t.Errorf("marked %d current, want 1", cur)
	}
	if !list[1].Current {
		t.Errorf("newest (active) not marked current: %+v", list)
	}
}

// TestSessions_LegacyMigration verifies a pre-sessions {chatID}.jsonl is
// picked up as a listable, readable session and the pointer is written — so
// an upgrade never loses an existing conversation.
func TestSessions_LegacyMigration(t *testing.T) {
	h := newTestHistory(t)
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a legacy file exactly as the pre-sessions code wrote it.
	legacy := filepath.Join(h.dir, "legacy-chat.jsonl")
	body := `{"role":"user","content":"old"}` + "\n" + `{"role":"assistant","content":"reply"}` + "\n"
	if err := os.WriteFile(legacy, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if h.Current("legacy-chat") != "" {
		t.Fatal("pointer should not exist before resolve")
	}
	got := h.Load("legacy-chat")
	if len(got) != 2 || got[0].Content != "old" {
		t.Errorf("Load legacy = %+v, want old/reply", got)
	}
	sid := h.Current("legacy-chat")
	if sid == "" || !strings.HasPrefix(sid, "legacy-") {
		t.Errorf("Current after migrate = %q, want legacy-* id", sid)
	}
	if _, err := os.Stat(legacy); err == nil {
		t.Error("legacy file still present after migration")
	}
	list, _ := h.ListSessions("legacy-chat")
	if len(list) != 1 || list[0].ID != sid || !list[0].Current {
		t.Errorf("List = %+v, want migrated %q as current", list, sid)
	}
}

// TestSessions_NilSafe verifies every session method is safe on a nil History
// (memory disabled): either a no-op or an error, never a panic.
func TestSessions_NilSafe(t *testing.T) {
	var h *History
	if got := h.Current("c"); got != "" {
		t.Errorf("nil Current = %q, want empty", got)
	}
	if _, err := h.NewSession("c"); err == nil {
		t.Error("nil NewSession: want error")
	}
	if _, err := h.ListSessions("c"); err == nil {
		t.Error("nil ListSessions: want error")
	}
	if err := h.UseSession("c", "x"); err == nil {
		t.Error("nil UseSession: want error")
	}
	if err := h.DeleteSession("c", ""); err == nil {
		t.Error("nil DeleteSession: want error")
	}
}

// TestValidSessionID verifies the charset gate that keeps user-supplied ids
// inside the history directory.
func TestValidSessionID(t *testing.T) {
	good := []string{
		"20260101-120000",
		"20260101-120000-123456789", // nanosecond disambiguator
		"legacy-20260101-120000",
		"abc123",
	}
	for _, s := range good {
		if !validSessionID(s) {
			t.Errorf("validSessionID(%q) = false, want true", s)
		}
	}
	bad := []string{
		"",                   // empty
		"..",                 // dot traversal
		"a/b",                // path separator
		"UPPER",              // uppercase
		"with space",         // whitespace
		"under_score",        // underscore not in the produced charset
		"semi;colon",
		strings.Repeat("a", 65), // over length cap
	}
	for _, s := range bad {
		if validSessionID(s) {
			t.Errorf("validSessionID(%q) = true, want false", s)
		}
	}
}
