package feishufront

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
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
	for i := 0; i < n; i++ {
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
