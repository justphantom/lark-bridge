package bridgebase

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAbsDir(t *testing.T) {
	if err := ValidateAbsDir("relative/path"); err == nil ||
		!strings.Contains(err.Error(), "绝对路径") {
		t.Fatalf("relative must be rejected: %v", err)
	}
	if err := ValidateAbsDir("/nonexistent-dir-xyz-abc-123"); err == nil {
		t.Fatal("missing dir must be rejected")
	}
	// Existing writable dir.
	tmp := t.TempDir()
	abs := filepath.Join(tmp, "sub")
	if err := os.MkdirAll(abs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAbsDir(abs); err != nil {
		t.Fatalf("writable dir: %v", err)
	}
	// A regular file is not a directory.
	f := filepath.Join(tmp, "file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateAbsDir(f); err == nil ||
		!strings.Contains(err.Error(), "不是目录") {
		t.Fatalf("file must be rejected: %v", err)
	}
}

func TestValidateSessionDirPath(t *testing.T) {
	if err := ValidateSessionDirPath("rel"); err == nil {
		t.Fatal("relative rejected")
	}
	if err := ValidateSessionDirPath("/abs/path"); err != nil {
		t.Fatalf("absolute OK: %v", err)
	}
}

func TestCreateSessionDir(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "a", "b", "c")
	if err := CreateSessionDir(target); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatal("not a directory")
	}
	// Perm bits should match DirPerm (modulo umask applied to subdirs; the
	// leaf dir is created by MkdirAll with the explicit mode).
	if info.Mode().Perm() != DirPerm {
		t.Fatalf("perm = %v, want %v", info.Mode().Perm(), DirPerm)
	}
}
