package fileconvert

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestConvert_XlsxRejectedFromConvert(t *testing.T) {
	// xlsx must go through ConvertXlsx (C-paradigm metadata return); the
	// generic Convert entry refuses it so metadata is never silently dropped.
	c := New(Options{})
	src, _ := writeSrc(t, "data.xlsx", "x")
	err := c.Convert(context.Background(), src, filepath.Join(t.TempDir(), "out.md"))
	if err == nil || !strings.Contains(err.Error(), "ConvertXlsx") {
		t.Fatalf("got err=%v, want ConvertXlsx guard", err)
	}
}
