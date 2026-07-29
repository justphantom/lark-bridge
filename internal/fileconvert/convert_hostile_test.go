package fileconvert

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeHostileDocx builds a docx with the given parts (hostile-input tests
// author the malformed OOXML directly; the happy-path fixture helpers in
// convert_docx_test.go cannot express negative levels etc).
func writeHostileDocx(t *testing.T, parts map[string]string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "hostile.docx")
	f, err := os.Create(src)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	zw := zip.NewWriter(f)
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return src
}

// A negative ilvl on a numbering.xml style-linked lvl must be floored to 0,
// not panic strings.Repeat and kill the whole conversion.
func TestConvertDocx_NegativeIlvlStyleLink(t *testing.T) {
	src := writeHostileDocx(t, map[string]string{
		"word/numbering.xml": `<?xml version="1.0?>
<w:numbering xmlns:w="` + nsW + `">
  <w:abstractNum w:abstractNumId="1">
    <w:lvl w:ilvl="-1"><w:start w:val="1"/><w:numFmt w:val="bullet"/><w:pStyle w:val="EvilList"/></w:lvl>
    <w:lvl w:ilvl="0"><w:start w:val="1"/><w:numFmt w:val="bullet"/></w:lvl>
  </w:abstractNum>
  <w:num w:numId="7"><w:abstractNumId w:val="1"/></w:num>
</w:numbering>`,
		"word/styles.xml": `<?xml version="1.0?>
<w:styles xmlns:w="` + nsW + `">
  <w:style w:type="paragraph" w:styleId="EvilList"><w:name w:val="Evil List"/></w:style>
</w:styles>`,
		"word/document.xml": `<?xml version="1.0?>
<w:document xmlns:w="` + nsW + `"><w:body>
  <w:p><w:pPr><w:pStyle w:val="EvilList"/></w:pPr><w:r><w:t>hello</w:t></w:r></w:p>
</w:body></w:document>`,
	})
	dst := filepath.Join(t.TempDir(), "out.md")
	if err := New(Options{}).Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("negative style-linked ilvl must not fail conversion: %v", err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !strings.Contains(string(b), "hello") {
		t.Errorf("paragraph content lost:\n%s", b)
	}
}

// Same protection for a negative ilvl in styles.xml's own numPr.
func TestConvertDocx_NegativeIlvlStyleNumPr(t *testing.T) {
	src := writeHostileDocx(t, map[string]string{
		"word/styles.xml": `<?xml version="1.0?>
<w:styles xmlns:w="` + nsW + `">
  <w:style w:type="paragraph" w:styleId="EvilList">
    <w:name w:val="Evil List"/>
    <w:pPr><w:numPr><w:ilvl w:val="-3"/><w:numId w:val="7"/></w:numPr></w:pPr>
  </w:style>
</w:styles>`,
		"word/numbering.xml": `<?xml version="1.0?>
<w:numbering xmlns:w="` + nsW + `">
  <w:abstractNum w:abstractNumId="1">
    <w:lvl w:ilvl="0"><w:start w:val="1"/><w:numFmt w:val="bullet"/></w:lvl>
  </w:abstractNum>
  <w:num w:numId="7"><w:abstractNumId w:val="1"/></w:num>
</w:numbering>`,
		"word/document.xml": `<?xml version="1.0?>
<w:document xmlns:w="` + nsW + `"><w:body>
  <w:p><w:pPr><w:pStyle w:val="EvilList"/></w:pPr><w:r><w:t>hello</w:t></w:r></w:p>
</w:body></w:document>`,
	})
	dst := filepath.Join(t.TempDir(), "out.md")
	if err := New(Options{}).Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("negative styles.xml numPr ilvl must not fail conversion: %v", err)
	}
}

// A bullet item whose text opens with ``` must be escaped so GFM does not
// open a fenced block inside the list item.
func TestPptx_BulletFenceEscaped(t *testing.T) {
	var buf bytes.Buffer
	p := &slideParser{bw: &buf}
	p.bullet = "char"
	p.emitParagraph("```bash")
	got := buf.String()
	if !strings.HasPrefix(got, "- \\```bash") {
		t.Errorf("bullet fence not escaped: %q", got)
	}
}

// parseSheet must surface the padding budget as errSheetTooSparse, not a
// generic parser panic, so the caveat message is actionable.
func TestParseSheet_PaddingBudgetError(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<worksheet ` + sDecl + `><sheetData>`)
	for range 30000 {
		sb.WriteString(`<row><c r="A1"><v>1</v></c></row>`)
	}
	sb.WriteString(`<row><c r="XFD1"><v>2</v></c></row>`)
	sb.WriteString(`</sheetData></worksheet>`)
	_, err := parseSheet(context.Background(), []byte(sb.String()), nil, parseNumFmts(nil), false, "value")
	if !errors.Is(err, errSheetTooSparse) {
		t.Errorf("err = %v, want errSheetTooSparse", err)
	}
}
