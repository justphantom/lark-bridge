package bridgebase

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSafeJoin(t *testing.T) {
	root := t.TempDir()
	abs, _ := filepath.Abs(root)
	sub := filepath.Join(abs, "sub")
	must(t, os.Mkdir(sub, 0o700))
	must(t, os.WriteFile(filepath.Join(sub, "file.txt"), []byte("x"), 0o600))

	cases := []struct {
		name    string
		rel     string
		wantErr bool
	}{
		{"direct file", "sub/file.txt", false},
		{"nested via ..", "sub/../sub/file.txt", false},
		{"escape via ..", "../../etc/passwd", true},
		{"missing", "nope.txt", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SafeJoin(abs, c.rel)
			if c.wantErr {
				if err == nil {
					t.Errorf("SafeJoin(%q) = %q, want error", c.rel, got)
				}
				return
			}
			if err != nil {
				t.Errorf("SafeJoin(%q): %v", c.rel, err)
			}
		})
	}
}

func TestSafeJoin_SymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behaviour on Windows differs; EvalSymlinks needs privileges")
	}
	root := t.TempDir()
	abs, _ := filepath.Abs(root)
	outside := t.TempDir()
	must(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600))
	// root/link → outside (escape)
	if err := os.Symlink(outside, filepath.Join(abs, "link")); err != nil {
		t.Skipf("symlink create failed (privileges?): %v", err)
	}
	if _, err := SafeJoin(abs, "link"); err == nil {
		t.Errorf("SafeJoin via symlink escaping root should fail")
	}
}

func TestBuildSendOptions(t *testing.T) {
	root := t.TempDir()
	abs, _ := filepath.Abs(root)
	must(t, os.Mkdir(filepath.Join(abs, "beta"), 0o700))
	must(t, os.Mkdir(filepath.Join(abs, "alpha"), 0o700))
	must(t, os.WriteFile(filepath.Join(abs, "z.txt"), []byte("z"), 0o600))
	must(t, os.WriteFile(filepath.Join(abs, "a.txt"), []byte("a"), 0o600))
	must(t, os.WriteFile(filepath.Join(abs, ".hidden"), []byte("h"), 0o600))

	entries, err := os.ReadDir(abs)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	// At root: no ".."; dirs sorted first, then files; dotfile hidden.
	got := BuildSendOptions(abs, abs, entries)
	want := []string{"📁 alpha/", "📁 beta/", "📄 a.txt", "📄 z.txt"}
	if len(got) != len(want) {
		t.Fatalf("at root got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("at root [%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	for _, o := range got {
		if o == "⬆️ .." {
			t.Errorf("root should not list ..")
		}
	}

	// One level deep: ".." leads.
	sub := filepath.Join(abs, "alpha")
	must(t, os.WriteFile(filepath.Join(sub, "inner.txt"), []byte("i"), 0o600))
	subEntries, _ := os.ReadDir(sub)
	gotSub := BuildSendOptions(sub, abs, subEntries)
	if len(gotSub) == 0 || gotSub[0] != "⬆️ .." {
		t.Errorf("subdir should list .. first, got %v", gotSub)
	}
}

func TestBuildSendOptions_Truncates(t *testing.T) {
	root := t.TempDir()
	abs, _ := filepath.Abs(root)
	// Create more entries than sendDirOptionLimit (100) so the cap engages.
	for i := range sendDirOptionLimit + 5 {
		must(t, os.WriteFile(filepath.Join(abs, fmt.Sprintf("f%03d.txt", i)), []byte("x"), 0o600))
	}
	entries, _ := os.ReadDir(abs)
	got := BuildSendOptions(abs, abs, entries)
	if len(got) != sendDirOptionLimit {
		t.Errorf("got %d options, want cap %d", len(got), sendDirOptionLimit)
	}
}

func TestParseSendOption(t *testing.T) {
	cases := []struct {
		in   string
		kind string
		name string
	}{
		{"⬆️ ..", "up", ""},
		{"📁 projects/", "dir", "projects"},
		{"📄 notes.md", "file", "notes.md"},
		{"unknown", "", ""},
	}
	for _, c := range cases {
		kind, name := ParseSendOption(c.in)
		if kind != c.kind || name != c.name {
			t.Errorf("ParseSendOption(%q) = (%q,%q), want (%q,%q)", c.in, kind, name, c.kind, c.name)
		}
	}
}

func TestReadFilePayload_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	original := []byte("hello\x00binary\n世界")
	must(t, os.WriteFile(path, original, 0o600))

	p, err := ReadFilePayload("oc_chat", "data.bin", path, "om_card")
	if err != nil {
		t.Fatalf("ReadFilePayload: %v", err)
	}
	if p.ChatID != "oc_chat" || p.FileName != "data.bin" || p.UpdateMessageID != "om_card" {
		t.Errorf("metadata wrong: %+v", p)
	}
	decoded, err := base64.StdEncoding.DecodeString(p.Content)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != string(original) {
		t.Errorf("content round-trip mismatch: got %q want %q", decoded, original)
	}
}

func TestReadFilePayload_MissingFile(t *testing.T) {
	_, err := ReadFilePayload("c", "x", filepath.Join(t.TempDir(), "nope"), "")
	if err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestReadFilePayload_OverLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	// MaxSendFileSize + 1 bytes: write via a trimmed buffer in chunks to keep
	// the test memory footprint bounded while still crossing the cap.
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	chunk := make([]byte, 1<<20)
	remaining := int64(MaxSendFileSize + 1)
	for remaining > 0 {
		n := int64(len(chunk))
		if n > remaining {
			n = remaining
		}
		if _, werr := f.Write(chunk[:n]); werr != nil {
			t.Fatalf("write: %v", werr)
		}
		remaining -= n
	}
	_ = f.Close()

	_, err = ReadFilePayload("c", "big.bin", path, "")
	if err == nil {
		t.Errorf("expected over-limit error")
	} else if !strings.Contains(err.Error(), "上限") {
		t.Errorf("over-limit error should mention cap, got: %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%v", err)
	}
}
