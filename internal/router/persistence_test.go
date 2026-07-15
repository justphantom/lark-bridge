package router

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hu/lark-bridge/internal/log"
)

type fakeCreator struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeCreator) CreateSessionInDirectory(ctx context.Context, title, directory string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return fmt.Sprintf("session-%d-%s", f.calls, directory), "", nil
}

// writeRaw writes arbitrary bytes to the router file so tests can craft
// degenerate payloads (missing key, explicit null, ...).
func writeRaw(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write raw: %v", err)
	}
}

func TestLoadMissingBindingsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	writeRaw(t, path, `{"version":5}`)

	r, err := New(&fakeCreator{}, path, log.Nop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()
	r.Bind("c1", "s1", "/d", "", "", "")
	if got, ok := r.Lookup("c1"); !ok || got.SessionID != "s1" {
		t.Fatalf("expected c1 binding, got %+v ok=%v", got, ok)
	}
}

func TestLoadExplicitNullBindings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	writeRaw(t, path, `{"version":5,"bindings":null}`)

	r, err := New(&fakeCreator{}, path, log.Nop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer r.Close()
	r.Bind("c1", "s1", "", "", "", "")
	if got, ok := r.Lookup("c1"); !ok || got.SessionID != "s1" {
		t.Fatalf("expected c1 binding, got %+v ok=%v", got, ok)
	}
}

func TestLoadEmptyBindings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	writeRaw(t, path, `{"version":5,"bindings":{}}`)

	r, _ := New(&fakeCreator{}, path, log.Nop())
	defer r.Close()
	if _, ok := r.Lookup("c1"); ok {
		t.Fatal("expected no binding for c1 in empty bindings")
	}
}

// TestLoadMalformedFails verifies a structurally broken router file fails
// loudly at startup rather than being silently reset to empty.
func TestLoadMalformedFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	writeRaw(t, path, `{"version":5,"bindings":`) // truncated JSON

	if _, err := New(&fakeCreator{}, path, log.Nop()); err == nil {
		t.Fatal("expected error loading malformed router file, got nil")
	}
}

// TestLoadUnsupportedVersionFails verifies an unsupported version is a hard
// error so an upgrade cannot silently wipe every binding.
func TestLoadUnsupportedVersionFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	writeRaw(t, path, `{"version":4,"bindings":{}}`)

	if _, err := New(&fakeCreator{}, path, log.Nop()); err == nil {
		t.Fatal("expected error for unsupported version, got nil")
	}
}

// TestClosePersistsLastMutation verifies that a Bind immediately followed by
// Close survives to the next New. Covers both backends' fields.
func TestClosePersistsLastMutation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")

	r1, _ := New(&fakeCreator{}, path, log.Nop())
	r1.Bind("c1", "s1", "/d", "updated-title", "m", "a")
	r1.SetModelSpec("c1", "updated-model")
	r1.SetPermissionMode("c1", "plan")
	r1.SetEffortLevel("c1", "high")
	r1.SetSettingsFile("c1", "${HOME}/.claude/kimi.json")
	r1.SetAgent("c1", "build")
	r1.UpdateTitle("c1", "updated-title")
	r1.Close()

	r2, _ := New(&fakeCreator{}, path, log.Nop())
	defer r2.Close()
	got, ok := r2.Lookup("c1")
	if !ok {
		t.Fatal("expected c1 binding after reload")
	}
	if got.SessionID != "s1" || got.Directory != "/d" ||
		got.ModelSpec != "updated-model" || got.Title != "updated-title" ||
		got.Agent != "build" ||
		got.PermissionMode != "plan" || got.EffortLevel != "high" ||
		got.SettingsFile != "${HOME}/.claude/kimi.json" {
		t.Fatalf("unexpected binding after reload: %+v", got)
	}
}

// TestLoadIgnoresCrossBackendFields verifies a v5 file written by one backend
// loads cleanly when read by the other (json.Unmarshal ignores unknown
// fields). A claude file with agent/lastUserMsgID still maps chatID→binding.
func TestLoadIgnoresCrossBackendFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	writeRaw(t, path, `{"version":5,"bindings":{"c1":{"sessionID":"s1","directory":"/d","title":"t","modelSpec":"m","agent":"build","lastUserMsgID":"om_xxx","permissionMode":"plan","effortLevel":"high"}}}`)

	r, _ := New(&fakeCreator{}, path, log.Nop())
	defer r.Close()
	got, ok := r.Lookup("c1")
	if !ok {
		t.Fatal("expected c1 binding")
	}
	if got.SessionID != "s1" || got.ModelSpec != "m" || got.PermissionMode != "plan" || got.Agent != "build" {
		t.Fatalf("cross-backend tolerance failed: %+v", got)
	}
}

// TestGetOrCreateCreatesBinding verifies the opencode GetOrCreate path
// produces a binding via the injected SessionCreator.
func TestGetOrCreateCreatesBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	r, _ := New(&fakeCreator{}, path, log.Nop())
	defer r.Close()

	b, err := r.GetOrCreate(context.Background(), "c1", "/d", "t", "m", "a")
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if b.SessionID == "" || b.Directory != "/d" || b.ModelSpec != "m" || b.Agent != "a" {
		t.Fatalf("unexpected binding: %+v", b)
	}
	// Second call returns the existing binding (no new session created).
	b2, err := r.GetOrCreate(context.Background(), "c1", "/d2", "t2", "m2", "a2")
	if err != nil {
		t.Fatalf("GetOrCreate second: %v", err)
	}
	if b2.SessionID != b.SessionID {
		t.Fatalf("second call created a new session: %q vs %q", b2.SessionID, b.SessionID)
	}
}

// TestNilCreatorClaudePath verifies a router constructed with a nil
// SessionCreator (the claude-back path) still supports Bind/Lookup/Set*.
func TestNilCreatorClaudePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")
	r, _ := New(nil, path, log.Nop())
	defer r.Close()

	r.Bind("c1", "", "/d", "t", "m", "")
	r.SetSessionID("c1", "s1")
	r.SetPermissionMode("c1", "plan")
	got, ok := r.Lookup("c1")
	if !ok || got.SessionID != "s1" || got.PermissionMode != "plan" {
		t.Fatalf("claude path failed: %+v ok=%v", got, ok)
	}
}

// TestCloseWithConcurrentBind verifies no race when Bind runs concurrently
// with Close (-race in CI validates).
func TestCloseWithConcurrentBind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.v5.json")

	r, _ := New(&fakeCreator{}, path, log.Nop())

	var wg sync.WaitGroup
	done := make(chan struct{})
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					r.Bind("c1", "s1", "/d", "t", "m", "a")
				}
			}
		}()
	}
	r.Close()
	close(done)
	wg.Wait()
}
