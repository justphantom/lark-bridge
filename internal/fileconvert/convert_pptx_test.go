package fileconvert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Slide content builders. A pptx is a zip of OOXML parts, so fixtures are
// authored inline as XML — reproducible and dependency-free, sidestepping the
// design's "MS PowerPoint export" requirement for the test suite (§6.1).

const relSlide = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide"

func presentationXML(rids []string) string {
	var b strings.Builder
	b.WriteString(`<p:presentation xmlns:a="` + nsA + `" xmlns:p="` + nsP + `" xmlns:r="` + ooRelsNS + `"><p:sldIdLst>`)
	for _, rid := range rids {
		// id is the slide's own id; r:id is the relationship to slideN.xml.
		// They may differ but only r:id drives ordering, so reuse the value.
		b.WriteString(`<p:sldId id="1" r:id="` + rid + `"/>`)
	}
	b.WriteString(`</p:sldIdLst></p:presentation>`)
	return b.String()
}

func presRelsXML(targets map[string]string) string {
	var b strings.Builder
	b.WriteString(`<Relationships ` + pkgRelsNS + `>`)
	for rid, target := range targets {
		b.WriteString(relXML(rid, relSlide, target))
	}
	b.WriteString(`</Relationships>`)
	return b.String()
}

func slideXML(inner string) string {
	return `<p:sld xmlns:a="` + nsA + `" xmlns:p="` + nsP + `" xmlns:r="` + ooRelsNS + `"><p:cSld><p:spTree>` + inner + `</p:spTree></p:cSld></p:sld>`
}

func titleShape(text string) string {
	return `<p:sp><p:nvSpPr><p:cNvPr id="2" name="Title 1"/><p:cNvSpPr/><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>` + text + `</a:t></a:r></a:p></p:txBody></p:sp>`
}

func bulletShape(items []string) string {
	var ps []string
	for _, it := range items {
		ps = append(ps, `<a:p><a:pPr><a:buChar char="•"/></a:pPr><a:r><a:t>`+it+`</a:t></a:r></a:p>`)
	}
	return `<p:sp><p:nvSpPr><p:cNvPr id="3" name="Content 2"/><p:cNvSpPr/><p:nvPr><p:ph type="body"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/>` + strings.Join(ps, "") + `</p:txBody></p:sp>`
}

func orderedShape(items []string) string {
	var ps []string
	for range items {
		ps = append(ps, `<a:p><a:pPr><a:buAutoNum type="arabicPeriod"/></a:pPr><a:r><a:t>item</a:t></a:r></a:p>`)
	}
	return `<p:sp><p:nvSpPr><p:cNvPr id="3" name="Content 2"/><p:cNvSpPr/><p:nvPr><p:ph type="body"/></p:nvPr></p:nvSpPr><p:spPr/><p:txBody><a:bodyPr/><a:lstStyle/>` + strings.Join(ps, "") + `</p:txBody></p:sp>`
}

func chartFrame() string {
	return `<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="4" name="Chart 3"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr><p:xfrm/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/chart"><c:chart xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"/></a:graphicData></a:graphic></p:graphicFrame>`
}

func smartArtFrame() string {
	return `<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="5" name="Diagram 4"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr><p:xfrm/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/diagram"><dgm:diag xmlns:dgm="http://schemas.openxmlformats.org/drawingml/2006/diagram"/></a:graphicData></a:graphic></p:graphicFrame>`
}

func tableFrame(rows [][]string) string {
	var trs []string
	for _, r := range rows {
		var tcs []string
		for _, c := range r {
			tcs = append(tcs, `<a:tc><a:txBody><a:bodyPr/><a:p><a:r><a:t>`+c+`</a:t></a:r></a:p></a:txBody></a:tc>`)
		}
		trs = append(trs, `<a:tr>`+strings.Join(tcs, "")+`</a:tr>`)
	}
	return `<p:graphicFrame><p:nvGraphicFramePr><p:cNvPr id="6" name="Table 5"/><p:cNvGraphicFramePr/><p:nvPr/></p:nvGraphicFramePr><p:xfrm/><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/table"><a:tbl><a:tblPr/><a:tblGrid/>` + strings.Join(trs, "") + `</a:tbl></a:graphicData></a:graphic></p:graphicFrame>`
}

func convertPptxTo(t *testing.T, c *Converter, src string) string {
	t.Helper()
	dst := strings.TrimSuffix(src, ".pptx") + ".md"
	if err := c.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("Convert pptx: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	return string(body)
}

func TestConvertPptx_TitleAndBullets(t *testing.T) {
	src := filepath.Join(t.TempDir(), "t.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML([]string{"rId1"}),
		"ppt/_rels/presentation.xml.rels": presRelsXML(map[string]string{"rId1": "slides/slide1.xml"}),
		"ppt/slides/slide1.xml":           slideXML(titleShape("季度回顾") + bulletShape([]string{"增长 23%", "新增客户 1200"})),
	})
	body := convertPptxTo(t, New(Options{}), src)
	for _, want := range []string{"## Slide 1", "# 季度回顾", "- 增长 23%", "- 新增客户 1200"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}

func TestConvertPptx_OrderedListNumbered(t *testing.T) {
	src := filepath.Join(t.TempDir(), "o.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML([]string{"rId1"}),
		"ppt/_rels/presentation.xml.rels": presRelsXML(map[string]string{"rId1": "slides/slide1.xml"}),
		"ppt/slides/slide1.xml":           slideXML(orderedShape([]string{"a", "b", "c"})),
	})
	body := convertPptxTo(t, New(Options{}), src)
	if !strings.Contains(body, "1. item") || !strings.Contains(body, "2. item") || !strings.Contains(body, "3. item") {
		t.Errorf("ordered list not numbered sequentially:\n%s", body)
	}
}

func TestConvertPptx_TableExtracted(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tbl.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML([]string{"rId1"}),
		"ppt/_rels/presentation.xml.rels": presRelsXML(map[string]string{"rId1": "slides/slide1.xml"}),
		"ppt/slides/slide1.xml": slideXML(tableFrame([][]string{
			{"区域", "销售额"},
			{"华东", "¥1.2M"},
		})),
	})
	body := convertPptxTo(t, New(Options{}), src)
	if !strings.Contains(body, "| 区域 | 销售额 |") {
		t.Errorf("table header missing:\n%s", body)
	}
	if !strings.Contains(body, "| 华东 | ¥1.2M |") {
		t.Errorf("table body missing:\n%s", body)
	}
}

func TestConvertPptx_ChartPlaceholder(t *testing.T) {
	src := filepath.Join(t.TempDir(), "chart.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML([]string{"rId1"}),
		"ppt/_rels/presentation.xml.rels": presRelsXML(map[string]string{"rId1": "slides/slide1.xml"}),
		"ppt/slides/slide1.xml":           slideXML(titleShape("X") + chartFrame()),
	})
	body := convertPptxTo(t, New(Options{}), src)
	if !strings.Contains(body, "<!-- Slide 1: 含 1 个图表（chart），未提取 -->") {
		t.Errorf("chart placeholder missing:\n%s", body)
	}
}

func TestConvertPptx_SmartArtPlaceholder(t *testing.T) {
	src := filepath.Join(t.TempDir(), "dgm.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML([]string{"rId1"}),
		"ppt/_rels/presentation.xml.rels": presRelsXML(map[string]string{"rId1": "slides/slide1.xml"}),
		"ppt/slides/slide1.xml":           slideXML(smartArtFrame()),
	})
	body := convertPptxTo(t, New(Options{}), src)
	if !strings.Contains(body, "<!-- Slide 1: 含 SmartArt，未提取 -->") {
		t.Errorf("SmartArt placeholder missing:\n%s", body)
	}
}

func TestConvertPptx_SlideOrderFollowsSldIdLst(t *testing.T) {
	// sldIdLst lists rId2 before rId1; slide1.xml=Alpha, slide2.xml=Beta.
	// Output must follow sldIdLst (Beta then Alpha), not filename order.
	src := filepath.Join(t.TempDir(), "order.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML([]string{"rId2", "rId1"}),
		"ppt/_rels/presentation.xml.rels": presRelsXML(map[string]string{"rId1": "slides/slide1.xml", "rId2": "slides/slide2.xml"}),
		"ppt/slides/slide1.xml":           slideXML(titleShape("Alpha")),
		"ppt/slides/slide2.xml":           slideXML(titleShape("Beta")),
	})
	body := convertPptxTo(t, New(Options{}), src)
	iBeta, iAlpha := strings.Index(body, "Beta"), strings.Index(body, "Alpha")
	if iBeta < 0 || iAlpha < 0 {
		t.Fatalf("missing slide titles:\n%s", body)
	}
	if iBeta >= iAlpha {
		t.Errorf("Beta should precede Alpha (sldIdLst order):\n%s", body)
	}
}

func TestConvertPptx_MaxSlidesCap(t *testing.T) {
	src := filepath.Join(t.TempDir(), "cap.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML([]string{"rId1", "rId2"}),
		"ppt/_rels/presentation.xml.rels": presRelsXML(map[string]string{"rId1": "slides/slide1.xml", "rId2": "slides/slide2.xml"}),
		"ppt/slides/slide1.xml":           slideXML(titleShape("One")),
		"ppt/slides/slide2.xml":           slideXML(titleShape("Two")),
	})
	c := New(Options{PptxMaxSlides: 1})
	dst := strings.TrimSuffix(src, ".pptx") + ".md"
	if err := c.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("Convert: %v", err)
	}
	body, _ := os.ReadFile(dst)
	if !strings.Contains(string(body), "## Slide 1") {
		t.Errorf("slide 1 missing:\n%s", body)
	}
	if strings.Contains(string(body), "## Slide 2") {
		t.Errorf("slide 2 should be capped out:\n%s", body)
	}
}

func TestConvertPptx_EmptyPresentation(t *testing.T) {
	src := filepath.Join(t.TempDir(), "empty.pptx")
	writeZipXlsx(t, src, map[string]string{
		"ppt/presentation.xml":            presentationXML(nil),
		"ppt/_rels/presentation.xml.rels": presRelsXML(nil),
	})
	c := New(Options{})
	dst := strings.TrimSuffix(src, ".pptx") + ".md"
	if err := c.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("empty pptx should convert without error, got: %v", err)
	}
}

func TestConvertPptx_CorruptFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bad.pptx")
	if err := os.WriteFile(src, []byte("not a zip"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := New(Options{})
	if err := c.Convert(context.Background(), src, src+".md"); err == nil {
		t.Errorf("expected error on corrupt pptx, got nil")
	}
}
