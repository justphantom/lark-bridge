package bridgebase

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDirCache_Validate covers the workspace containment guard.
func TestDirCache_Validate(t *testing.T) {
	root := "/home/user/projects"
	cases := []struct {
		name    string
		dir     string
		root    string
		wantErr bool
	}{
		{"empty root rejects all", "/home/user/projects/a", "", true},
		{"direct child ok", "/home/user/projects/a", root, false},
		{"nested child ok", "/home/user/projects/a/b/c", root, false},
		{"sibling outside root rejected", "/home/user/other", root, true},
		{"parent traversal rejected", "/home/user/projects/../../etc", root, true},
		{"unrelated absolute rejected", "/etc/passwd", root, true},
		{"root itself ok (rel=.)", root, root, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := NewDirCache(c.root).Validate(c.dir)
			if c.wantErr && err == nil {
				t.Errorf("Validate(%q) with root %q expected error", c.dir, c.root)
			}
			if !c.wantErr && err != nil {
				t.Errorf("Validate(%q) with root %q unexpected error: %v", c.dir, c.root, err)
			}
		})
	}
}

// TestDirCache_List scans immediate subdirectories, sorted, skipping files.
func TestDirCache_List(t *testing.T) {
	workspace := t.TempDir()
	os.MkdirAll(filepath.Join(workspace, "proj-a"), 0o755)
	os.MkdirAll(filepath.Join(workspace, "proj-b"), 0o755)
	os.WriteFile(filepath.Join(workspace, "not-a-dir.txt"), []byte("x"), 0o644) // skipped

	dirs, err := NewDirCache(workspace).List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("got %d dirs, want 2: %v", len(dirs), dirs)
	}
	if filepath.Base(dirs[0]) != "proj-a" || filepath.Base(dirs[1]) != "proj-b" {
		t.Errorf("dirs not sorted or wrong: %v", dirs)
	}
}

// TestDirCache_ListEmptyRoot verifies an unset root errors.
func TestDirCache_ListEmptyRoot(t *testing.T) {
	if _, err := NewDirCache("").List(); err == nil {
		t.Fatal("expected error for empty root")
	}
}
