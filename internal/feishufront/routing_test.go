package feishufront

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestLayer1Router_UnboundReturnsError(t *testing.T) {
	r, err := NewLayer1Router("")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := r.Resolve("c1"); err == nil {
		t.Fatal("expected error for unbound chat")
	}
}

func TestLayer1Router_Set(t *testing.T) {
	r, _ := NewLayer1Router("")
	if err := r.Set("c1", "opencode-1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := r.Resolve("c1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "opencode-1" {
		t.Errorf("Resolve after Set = %q, want opencode-1", got)
	}
}

func TestLayer1Router_ChatsOf(t *testing.T) {
	r, _ := NewLayer1Router("")
	_ = r.Set("c1", "b1")
	_ = r.Set("c2", "b1")
	_ = r.Set("c3", "b2")
	got := r.ChatsOf("b1")
	if len(got) != 2 {
		t.Errorf("ChatsOf(b1) = %v, want 2 chats", got)
	}
}

func TestLayer1Router_PersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing.json")
	r1, err := NewLayer1Router(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := r1.Set("c1", "opencode-1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Reload from disk; the mapping must survive.
	r2, err := NewLayer1Router(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, _ := r2.Resolve("c1"); got != "opencode-1" {
		t.Errorf("after reload Resolve(c1) = %q, want opencode-1", got)
	}
}

func TestLayer1Router_LoadMissingFile(t *testing.T) {
	// A non-existent path initialises an empty table, not an error.
	path := filepath.Join(t.TempDir(), "absent.json")
	r, err := NewLayer1Router(path)
	if err != nil {
		t.Fatalf("new with missing file: %v", err)
	}
	if _, err := r.Resolve("c1"); err == nil {
		t.Fatal("expected error for unbound chat on empty router")
	}
}

// TestLayer1Router_ConcurrentSetDoesNotCorruptFile exercises the saveMu
// serialization: concurrent Set calls share one temp file, so without
// serialization they race and either rename fails or the file is torn.
func TestLayer1Router_ConcurrentSetDoesNotCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing.json")
	r, err := NewLayer1Router(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	const n = 60
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			_ = r.Set("set-"+strconv.Itoa(i), "b")
		}(i)
	}
	wg.Wait()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var f routingFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("routing file is not valid JSON (corrupted): %v\n%s", err, data)
	}
	if _, ok := f.Routes["set-0"]; !ok {
		t.Errorf("Set binding lost after concurrent writes; routes=%v", f.Routes)
	}
}

// TestLayer1Router_PruneRemovesStale verifies Prune drops only entries whose
// lastAccess is older than the TTL. "fresh" was just Set (lastAccess=now);
// "stale" is hand-seeded with a 30d-old lastAccess.
func TestLayer1Router_PruneRemovesStale(t *testing.T) {
	r, _ := NewLayer1Router("")
	_ = r.Set("fresh", "b1")
	r.mu.Lock()
	old := time.Now().Add(-30 * 24 * time.Hour)
	r.routes["stale"] = routeEntry{BackendID: "b1", UpdatedAt: old.Unix(), lastAccess: old}
	r.mu.Unlock()

	if removed := r.Prune(14 * 24 * time.Hour); removed != 1 {
		t.Fatalf("Prune removed %d, want 1", removed)
	}
	if _, err := r.Resolve("stale"); err == nil {
		t.Error("stale entry should have been pruned")
	}
	if _, err := r.Resolve("fresh"); err != nil {
		t.Error("fresh entry should remain")
	}
}

// TestLayer1Router_ResolveRefreshesLastAccess verifies a Resolve hit bumps
// lastAccess so an otherwise-old entry is not pruned while still active.
func TestLayer1Router_ResolveRefreshesLastAccess(t *testing.T) {
	r, _ := NewLayer1Router("")
	old := time.Now().Add(-30 * 24 * time.Hour)
	r.mu.Lock()
	r.routes["c1"] = routeEntry{BackendID: "b1", UpdatedAt: old.Unix(), lastAccess: old}
	r.mu.Unlock()

	if _, err := r.Resolve("c1"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if removed := r.Prune(14 * 24 * time.Hour); removed != 0 {
		t.Errorf("Prune removed %d after Resolve, want 0 (Resolve refreshes lastAccess)", removed)
	}
}

// TestLayer1Router_LoadSeedsLastAccessFromUpdatedAt verifies that after a
// restart the in-memory lastAccess is seeded from the persisted UpdatedAt,
// so a chat bound long ago (and not yet Resolved) is prunable.
func TestLayer1Router_LoadSeedsLastAccessFromUpdatedAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing.json")
	stale := time.Now().Add(-30 * 24 * time.Hour).Unix()
	raw := `{"routes":{"c1":{"backendID":"b1","updatedAt":` + strconv.FormatInt(stale, 10) + `}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r, err := NewLayer1Router(path)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if removed := r.Prune(14 * 24 * time.Hour); removed != 1 {
		t.Errorf("Prune removed %d, want 1 (stale UpdatedAt should seed an old lastAccess)", removed)
	}
}

// TestLayer1Router_PrunePersists verifies a Prune that removes entries Save()s
// the pruned state, so a reload does not resurrect deleted bindings.
func TestLayer1Router_PrunePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "routing.json")
	r, _ := NewLayer1Router(path)
	_ = r.Set("keep", "b1")
	r.mu.Lock()
	old := time.Now().Add(-30 * 24 * time.Hour)
	r.routes["gone"] = routeEntry{BackendID: "b1", UpdatedAt: old.Unix(), lastAccess: old}
	r.mu.Unlock()

	if removed := r.Prune(14 * 24 * time.Hour); removed != 1 {
		t.Fatalf("Prune removed %d, want 1", removed)
	}
	r2, _ := NewLayer1Router(path)
	if _, err := r2.Resolve("gone"); err == nil {
		t.Error("pruned entry survived reload")
	}
	if _, err := r2.Resolve("keep"); err != nil {
		t.Error("kept entry lost after reload")
	}
}

// TestLayer1Router_PruneZeroTTLNoop verifies a non-positive TTL disables
// pruning (defensive guard, not a sweep of everything).
func TestLayer1Router_PruneZeroTTLNoop(t *testing.T) {
	r, _ := NewLayer1Router("")
	_ = r.Set("c1", "b1")
	if removed := r.Prune(0); removed != 0 {
		t.Errorf("Prune(0) removed %d, want 0", removed)
	}
	if _, err := r.Resolve("c1"); err != nil {
		t.Error("entry should remain after Prune(0)")
	}
}
