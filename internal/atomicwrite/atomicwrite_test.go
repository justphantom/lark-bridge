package atomicwrite

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteCreatesContentAndMode(t *testing.T) {
	// POSIX permission bits are not faithfully round-tripped on Windows
	// (the FS has no owner/group/other model), so the mode assertion is
	// Unix-only. The content/mode-write path itself still runs everywhere.
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit assertion is POSIX-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")

	if err := Write(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want %q", got, "hello")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 0644", perm)
	}
}

func TestWriteOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")

	if err := Write(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(path, []byte("new contents"), 0o600); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new contents" {
		t.Errorf("content = %q, want %q", got, "new contents")
	}
}

// A successful write must not leave the .tmp sibling behind: a crash
// mid-write that recovers into a clean state is the whole point of the
// tmp+rename dance.
func TestWriteLeavesNoTempResidue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")

	if err := Write(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp residue after success: stat err = %v", err)
	}
}

// Missing parent directory: OpenFile fails, no file is created, and the
// reported error is non-nil.
func TestWriteFailsOnMissingDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "f.txt")

	if err := Write(path, []byte("x"), 0o644); err == nil {
		t.Fatal("Write succeeded for a path under a missing directory, want error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("target created despite failure: stat err = %v", err)
	}
}
