package miniagent

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// TestListConfigFiles verifies the /config picker's directory scan: it returns
// miniagent.json and *-miniagent.json (absolute, sorted by basename), ignoring
// unrelated files, wrong-suffix lookalikes, and subdirectories.
func TestListConfigFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(name string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("miniagent.json")
	mustWrite("kimi-miniagent.json")
	mustWrite("aborted-miniagent.json")
	mustWrite("other.json")         // ignored: not a miniagent config
	mustWrite("miniagent.json.bak") // ignored: wrong suffix
	// A subdirectory named like a config must be skipped (IsDir guard).
	if err := os.Mkdir(filepath.Join(dir, "sub-miniagent.json"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	got, err := listConfigFiles(dir)
	if err != nil {
		t.Fatalf("listConfigFiles: %v", err)
	}
	want := []string{
		filepath.Join(dir, "aborted-miniagent.json"),
		filepath.Join(dir, "kimi-miniagent.json"),
		filepath.Join(dir, "miniagent.json"),
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}

	// Empty dir → no results, no error.
	if got, err := listConfigFiles(t.TempDir()); err != nil || len(got) != 0 {
		t.Errorf("empty dir: got %v, err %v, want nil/no-error", got, err)
	}
	// Empty path → nil, no error (defensive; no scan attempted).
	if got, err := listConfigFiles(""); err != nil || got != nil {
		t.Errorf("empty path: got %v, err %v, want nil/no-error", got, err)
	}
	// Nonexistent dir → error (surfaced to the user as a picker failure).
	if _, err := listConfigFiles(filepath.Join(dir, "nope")); err == nil {
		t.Errorf("nonexistent dir: want error, got nil")
	}
}
