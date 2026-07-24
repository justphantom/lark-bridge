package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_AddAccumulates(t *testing.T) {
	s, _ := New("", nil, 0)
	defer s.Close()

	s.Add(Delta{SessionID: "s1", ChatID: "c1", Input: 100, Output: 50, CacheRead: 200, Cost: 1.0, Turns: 1})
	s.Add(Delta{SessionID: "s1", Input: 30, Output: 20, CacheRead: 10, Cost: 0.5, Turns: 1})

	got := s.Snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 session, got %d", len(got))
	}
	e := got[0]
	if e.Input != 130 || e.Output != 70 || e.CacheRead != 210 || e.Cost != 1.5 || e.Turns != 2 {
		t.Errorf("entry = %+v", e)
	}
	if e.ChatID != "c1" {
		t.Errorf("ChatID = %q, want c1", e.ChatID)
	}
}

func TestStore_AddEmptySessionIgnored(t *testing.T) {
	s, _ := New("", nil, 0)
	defer s.Close()
	s.Add(Delta{SessionID: "", Input: 999})
	if len(s.Snapshot()) != 0 {
		t.Fatal("empty sessionID should be ignored")
	}
}

func TestStore_Get(t *testing.T) {
	s, _ := New("", nil, 0)
	defer s.Close()

	// Missing session.
	if _, ok := s.Get("nope"); ok {
		t.Fatal("Get on missing session should return ok=false")
	}

	s.Add(Delta{SessionID: "s1", ChatID: "c1", Input: 100, Output: 50, Turns: 1})
	e, ok := s.Get("s1")
	if !ok {
		t.Fatal("Get after Add should return ok=true")
	}
	if e.Input != 100 || e.Output != 50 || e.Turns != 1 || e.ChatID != "c1" {
		t.Errorf("entry = %+v", e)
	}

	// Get returns a copy: mutating it must not affect the store.
	e.Input = 999
	if e2, _ := s.Get("s1"); e2.Input != 100 {
		t.Fatal("mutating returned entry affected the store")
	}
}

func TestStore_GetNilSafe(t *testing.T) {
	var s *Store
	if _, ok := s.Get("s1"); ok {
		t.Fatal("Get on nil store should return ok=false, not panic")
	}
}

func TestStore_PersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage-test.json")

	s1, err := New(path, nil, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s1.Add(Delta{SessionID: "s1", ChatID: "c1", Input: 100, Output: 50, CacheRead: 200, CacheWrite: 5, Cost: 1.0, Turns: 1})
	s1.Close()

	// File should exist with version 1.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f fileShape
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Version != fileVersion {
		t.Errorf("version = %d, want %d", f.Version, fileVersion)
	}
	if f.Sessions["s1"].Input != 100 {
		t.Errorf("Input = %d, want 100", f.Sessions["s1"].Input)
	}

	// Reload and verify accumulation continues.
	s2, err := New(path, nil, 0)
	if err != nil {
		t.Fatalf("reload New: %v", err)
	}
	s2.Add(Delta{SessionID: "s1", Input: 50, Turns: 1})
	got := s2.Snapshot()[0]
	if got.Input != 150 || got.Turns != 2 {
		t.Errorf("after reload: Input=%d Turns=%d, want 150/2", got.Input, got.Turns)
	}
	s2.Close()
}

func TestStore_MissingFileNotError(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "nonexistent.json"), nil, 0)
	if err != nil {
		t.Fatalf("missing file should not error, got %v", err)
	}
	s.Close()
}

func TestStore_MalformedFileErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, err := New(path, nil, 0); err == nil {
		t.Fatal("malformed file should error")
	}
}

func TestStore_WrongVersionErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "v.json")
	os.WriteFile(path, []byte(`{"version":999,"sessions":{}}`), 0o600)
	if _, err := New(path, nil, 0); err == nil {
		t.Fatal("wrong version should error")
	}
}

// TestStore_CloseFlushesPendingSave verifies the Close final-save window:
// Add queues an async save on saveCh, but if Close runs before saveLoop
// drains it, Close's own close(saveStop)→<-saveDone→save() must still flush
// the data. Guards against a regression where the final save is skipped.
func TestStore_CloseFlushesPendingSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")

	s, err := New(path, nil, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Add(Delta{SessionID: "s1", ChatID: "c1", Input: 100, Output: 50, Cost: 1.0, Turns: 1})
	// Close immediately, racing the async saveLoop. The final save inside
	// Close must persist regardless of whether saveLoop drained saveCh.
	s.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file should exist after Close: %v", err)
	}
	var f fileShape
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.Sessions["s1"].Input != 100 || f.Sessions["s1"].Turns != 1 {
		t.Errorf("final save lost data: %+v", f.Sessions["s1"])
	}
}

// TestStore_CloseIsIdempotent verifies closeOnce: a second Close does not
// panic (double close of saveStop) or block (deadlock on saveDone already
// closed). Mirrors the production Close→main defer Close sequence.
func TestStore_CloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	s, err := New(path, nil, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.Close()
	s.Close() // must not panic or block
}

// TestStore_PruneRemovesExpired verifies the TTL sweep drops entries whose
// LastUpdate is older than ttl and keeps the rest. Without this sweep the
// sessions map and the persisted JSON would grow without bound over a long-
// running process (every new sessionID adds an entry that is never removed).
func TestStore_PruneRemovesExpired(t *testing.T) {
	s, _ := New("", nil, time.Hour)
	defer s.Close()

	// Add a fresh session, then age its LastUpdate back beyond ttl.
	s.Add(Delta{SessionID: "fresh", Input: 10, Turns: 1})
	s.Add(Delta{SessionID: "stale", Input: 20, Turns: 1})

	s.mu.Lock()
	old := s.sessions["stale"]
	old.LastUpdate = time.Now().Add(-2 * time.Hour) // older than the 1h ttl
	s.mu.Unlock()

	s.pruneLocked()

	if _, ok := s.Get("stale"); ok {
		t.Error("stale session was not pruned")
	}
	if _, ok := s.Get("fresh"); !ok {
		t.Error("fresh session was incorrectly pruned")
	}
}

// TestStore_PruneOnCloseFlushesToDisk verifies Close prunes before the final
// save, so a stale entry does not survive across process restarts.
func TestStore_PruneOnCloseFlushesToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")

	s1, _ := New(path, nil, time.Hour)
	s1.Add(Delta{SessionID: "stale", Input: 10, Turns: 1})
	s1.Add(Delta{SessionID: "fresh", Input: 20, Turns: 1})

	s1.mu.Lock()
	s1.sessions["stale"].LastUpdate = time.Now().Add(-2 * time.Hour)
	s1.mu.Unlock()

	s1.Close()

	data, _ := os.ReadFile(path)
	var f fileShape
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, exists := f.Sessions["stale"]; exists {
		t.Error("stale session persisted across Close (final save did not prune)")
	}
	if _, exists := f.Sessions["fresh"]; !exists {
		t.Error("fresh session was lost on Close")
	}
}
