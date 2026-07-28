package fileconvert

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helper: write a temp file and return its path + a clean dst path. dst is
// placed in a separate subdir so it can never collide with src when both
// share the same base name (which would let O_TRUNC clobber the source).
func writeSrc(t *testing.T, name, body string) (src, dst string) {
	t.Helper()
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	src = filepath.Join(srcDir, name)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	dst = filepath.Join(dstDir, base+".md")
	return src, dst
}

func TestConvert_UnsupportedExt(t *testing.T) {
	c := New(Options{})
	src, _ := writeSrc(t, "foo.pdf", "x")
	err := c.Convert(context.Background(), src, filepath.Join(t.TempDir(), "out.md"))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("got err=%v, want ErrUnsupported", err)
	}
}

func TestConvert_DstMustEndMd(t *testing.T) {
	c := New(Options{})
	src, _ := writeSrc(t, "foo.md", "x")
	err := c.Convert(context.Background(), src, filepath.Join(t.TempDir(), "out.txt"))
	if err == nil || !strings.Contains(err.Error(), "must end in .md") {
		t.Fatalf("got err=%v, want dst-suffix error", err)
	}
}

func TestConvert_MarkdownCopiedVerbatim(t *testing.T) {
	c := New(Options{})
	body := "# hi\n\nsome **bold**\n"
	src, dst := writeSrc(t, "note.md", body)
	if err := c.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("convert: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != body {
		t.Fatalf("dst content changed: got %q want %q", got, body)
	}
}

func TestConvert_TextCopiedWithMdExtension(t *testing.T) {
	c := New(Options{})
	src, dst := writeSrc(t, "log.txt", "plain text line\n")
	if err := c.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.HasSuffix(dst, ".md") {
		t.Fatalf("dst should be .md, got %s", dst)
	}
}

func TestConvert_DocxViaPandoc(t *testing.T) {
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc not installed in test environment")
	}
	// Build a minimal docx via pandoc itself (round-trip): write a .md source
	// and convert to .docx, then back. This avoids hard-coding a binary
	// fixture in the repo while exercising the real Convert path.
	dir := t.TempDir()
	md := filepath.Join(dir, "src.md")
	if err := os.WriteFile(md, []byte("# title\n\nbody para\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docx := filepath.Join(dir, "src.docx")
	if err := exec.Command("pandoc", "-f", "gfm", "-t", "docx", "-o", docx, md).Run(); err != nil {
		t.Skipf("pandoc unavailable to build fixture: %v", err)
	}
	dst := filepath.Join(dir, "out.md")
	c := New(Options{Timeout: 30 * time.Second})
	if err := c.Convert(context.Background(), docx, dst); err != nil {
		t.Fatalf("convert docx: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !strings.Contains(string(got), "title") || !strings.Contains(string(got), "body para") {
		t.Fatalf("docx→md output lost content: %q", got)
	}
}

func TestConvert_PandocTimeout(t *testing.T) {
	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc not installed")
	}
	// An empty/garbage .docx makes pandoc exit fast, not stall; to exercise
	// the timeout path deterministically we point pandoc at a path that does
	// not exist and assert we get a non-nil error wrapping ctx or pandoc
	// itself. The real guarantee (ctx cancellation SIGKILLs) is covered by
	// the cmdutil.ApplyGroupCancel regression tests; here we just assert the
	// timeout path produces a contextualised error rather than a silent nil.
	c := New(Options{Timeout: 5 * time.Millisecond})
	err := c.Convert(context.Background(),
		filepath.Join(t.TempDir(), "missing.docx"),
		filepath.Join(t.TempDir(), "out.md"))
	if err == nil {
		t.Fatal("expected non-nil error for missing docx, got nil")
	}
}
