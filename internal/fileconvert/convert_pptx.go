package fileconvert

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OOXML namespaces used by pptx slides. Matching on Space (not the prefix a
// producer happened to use) keeps parsing robust across MS Office / WPS /
// LibreOffice exports which all bind the same URIs to different prefixes.
const (
	nsP = "http://schemas.openxmlformats.org/presentationml/2006/main"
	nsA = "http://schemas.openxmlformats.org/drawingml/2006/main"
)

// convertPptx extracts textual content from a .pptx file into GFM Markdown
// (L1 scope, office-extract-design.md §2). Titles, body paragraphs, bullet
// lists and simple tables are recovered; layout, images, charts, SmartArt
// and animations are not. Charts/SmartArt emit HTML-comment placeholders so
// the agent knows what it cannot see (decision 9A); images are dropped
// silently (decision 3A). Slide order follows presentation.xml's sldIdLst,
// never the slideN.xml filename order.
func (c *Converter) convertPptx(ctx context.Context, srcPath, dstPath string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	base := filepath.Base(srcPath)
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("fileconvert: open pptx %s: %w", base, err)
	}
	defer func() { _ = zr.Close() }()

	parts := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		parts[zf.Name] = zf
	}
	slidePaths := orderedSlidePaths(parts)

	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileconvert: create pptx dst: %w", err)
	}
	bw := &bytes.Buffer{}
	// finishSlide writes the slide separator and flushes bw to out, bounding
	// peak memory to one slide's rendered body: a deck with many slides would
	// otherwise hold the entire rendered Markdown until a single final
	// WriteTo (M2). The buf's Write never fails; the only error (disk full /
	// unwritable) is checked once per slide here.
	finishSlide := func() error {
		bw.WriteString("---\n\n")
		if _, err := bw.WriteTo(out); err != nil {
			_ = out.Close()
			_ = os.Remove(dstPath)
			return fmt.Errorf("fileconvert: write pptx dst: %w", err)
		}
		bw.Reset()
		return nil
	}

	maxSlides := c.pptxMaxSlides
	for i, sp := range slidePaths {
		if maxSlides > 0 && i >= maxSlides {
			break
		}
		select {
		case <-ctx.Done():
			_ = out.Close()
			_ = os.Remove(dstPath)
			return fmt.Errorf("fileconvert: pptx %s cancelled: %w", base, ctx.Err())
		default:
		}
		fmt.Fprintf(bw, "## Slide %d\n\n", i+1)

		zf := parts[sp]
		if zf == nil {
			fmt.Fprintf(bw, "<!-- Slide %d: 缺失，未提取 -->\n\n", i+1)
			if err := finishSlide(); err != nil {
				return err
			}
			continue
		}
		data, rerr := readZipPart(zf)
		if rerr != nil {
			fmt.Fprintf(bw, "<!-- Slide %d: 读取失败，未提取: %s -->\n\n", i+1, sanitizeMetaText(rerr.Error()))
			if err := finishSlide(); err != nil {
				return err
			}
			continue
		}
		p := &slideParser{bw: bw}
		if perr := p.safeRun(data); perr != nil {
			// A panic on one hostile slide must surface (never silently
			// swallowed): leave a placeholder and a debug line, continue.
			fmt.Fprintf(bw, "<!-- Slide %d: 解析失败，部分内容可能丢失 -->\n\n", i+1)
			if c.log != nil {
				c.log.Debug("fileconvert: pptx slide parse panic",
					"src", base, "slide", i+1, "error", perr.Error())
			}
		}
		if p.charts > 0 {
			fmt.Fprintf(bw, "<!-- Slide %d: 含 %d 个图表（chart），未提取 -->\n\n", i+1, p.charts)
		}
		if p.diags > 0 {
			fmt.Fprintf(bw, "<!-- Slide %d: 含 SmartArt，未提取 -->\n\n", i+1)
		}
		if err := finishSlide(); err != nil {
			return err
		}
	}

	if err := out.Close(); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("fileconvert: close pptx dst: %w", err)
	}
	return nil
}

// orderedSlidePaths resolves presentation.xml's sldIdLst (the authoritative
// slide order) through presentation.xml.rels to concrete slideN.xml paths.
// Returns an empty slice for an empty/blank presentation.
func orderedSlidePaths(parts map[string]*zip.File) []string {
	var ids []string
	if pres := parts["ppt/presentation.xml"]; pres != nil {
		if data, err := readZipPart(pres); err == nil {
			ids = parseSldIdLst(data)
		}
	}
	rels := relsMap(parts, "ppt/_rels/presentation.xml.rels")
	out := make([]string, 0, len(ids))
	for _, rid := range ids {
		rel, ok := rels[rid]
		if !ok || rel.External {
			continue
		}
		out = append(out, resolvePartTarget("ppt", rel.Target))
	}
	return out
}

// parseSldIdLst returns the ordered r:id values of <p:sldId> entries. The r:id
// attribute lives in the relationships namespace, so the Space check is what
// distinguishes it from the unrelated sldId own "id" attribute.
func parseSldIdLst(data []byte) []string {
	var ids []string
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return ids
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sldId" {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "id" && a.Name.Space == ooRelsNS {
				ids = append(ids, a.Value)
			}
		}
	}
}

// slideParser is a streaming token-based extractor for one slide. It tracks
// just enough context — the current shape's placeholder type, the current
// paragraph's bullet flavour, and whether text is landing in a body paragraph
// or a table cell — to rebuild GFM without modelling the full OOXML tree.
type slideParser struct {
	bw *bytes.Buffer

	phType  string // placeholder type of the enclosing <p:sp>
	listNum int    // ordered-list counter, reset per shape

	inP    bool
	para   strings.Builder
	bullet string // "", "char", "autonum"
	inT    bool

	inTbl   bool
	rows    [][]string
	curRow  []string
	curCell strings.Builder
	inTc    bool

	gfURI  string // graphicData uri of the enclosing graphicFrame
	charts int
	diags  int
}

// safeRun runs run with a recover guard so one malformed slide never aborts
// the whole deck: anything already written stays, and the loop continues to
// the next slide (office-extract-design.md §5.3). The panic is returned as an
// error so the caller can leave a placeholder + log line — a silently
// swallowed panic would hide a parser bug and violate the "never silently
// skip" contract.
func (p *slideParser) safeRun(data []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser panic: %v", r)
		}
	}()
	p.run(data)
	return nil
}

func (p *slideParser) run(data []byte) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			p.start(t)
		case xml.EndElement:
			p.end(t)
		case xml.CharData:
			// Only collect inside <a:t>; OOXML inter-element whitespace would
			// otherwise pollute every paragraph.
			if p.inT {
				p.para.Write(t)
			}
		}
	}
}

func (p *slideParser) start(se xml.StartElement) {
	switch se.Name.Space {
	case nsP:
		switch se.Name.Local {
		case "sp":
			p.phType = ""
			p.listNum = 0
		case "ph":
			for _, a := range se.Attr {
				if a.Name.Local == "type" {
					p.phType = a.Value
				}
			}
		case "graphicFrame":
			p.gfURI = ""
		}
	case nsA:
		switch se.Name.Local {
		case "p":
			p.inP = true
			p.para.Reset()
			p.bullet = ""
		case "buChar":
			p.bullet = "char"
		case "buAutoNum":
			p.bullet = "autonum"
		case "t":
			p.inT = true
		case "br":
			if p.inP {
				p.para.WriteString("<br>")
			}
		case "tab":
			if p.inP {
				p.para.WriteString("\t")
			}
		case "graphicData":
			// graphicData lives in the drawingml namespace (a:), not p:. Its
			// uri attribute names the embedded object — chart / diagram /
			// table — classified when the enclosing p:graphicFrame closes.
			for _, a := range se.Attr {
				if a.Name.Local == "uri" {
					p.gfURI = a.Value
				}
			}
		case "tbl":
			p.inTbl = true
			p.rows = nil
		case "tr":
			p.curRow = nil
		case "tc":
			p.inTc = true
			p.curCell.Reset()
		}
	}
}

func (p *slideParser) end(se xml.EndElement) {
	switch se.Name.Space {
	case nsP:
		switch se.Name.Local {
		case "graphicFrame":
			switch {
			case strings.Contains(p.gfURI, "/chart"):
				p.charts++
			case strings.Contains(p.gfURI, "/diagram"):
				p.diags++
			}
			p.gfURI = ""
		}
	case nsA:
		switch se.Name.Local {
		case "p":
			p.inP = false
			text := strings.TrimSpace(p.para.String())
			if p.inTc {
				if p.curCell.Len() > 0 && text != "" {
					p.curCell.WriteString("<br>")
				}
				p.curCell.WriteString(text)
				return
			}
			p.emitParagraph(text)
		case "t":
			p.inT = false
		case "tc":
			p.inTc = false
			p.curRow = append(p.curRow, p.curCell.String())
		case "tr":
			if p.inTbl {
				p.rows = append(p.rows, p.curRow)
				p.curRow = nil
			}
		case "tbl":
			p.inTbl = false
			if len(p.rows) > 0 {
				p.bw.WriteString(renderTable(p.rows))
				p.bw.WriteByte('\n')
			}
			p.rows = nil
		}
	}
}

// emitParagraph writes one body paragraph with the right prefix: "#" / "##"
// for title/subtitle placeholders, "- " for bulleted lists, "N. " for auto-
// numbered lists, nothing otherwise. Empty paragraphs are dropped so a shape
// with blank lines does not spray stray newlines.
func (p *slideParser) emitParagraph(text string) {
	if text == "" {
		return
	}
	prefix := ""
	switch p.phType {
	case "title", "ctrTitle":
		prefix = "# "
	case "subTitle":
		prefix = "## "
	}
	switch p.bullet {
	case "char":
		prefix = "- "
	case "autonum":
		p.listNum++
		prefix = fmt.Sprintf("%d. ", p.listNum)
	}
	// escapeLeading applies in every branch (not just plain paragraphs):
	// bullet/numbered text opening with ``` or "- " would otherwise open a
	// fence or a nested list inside the item; after a "#" prefix the escape
	// is harmless.
	text = escapeLeading(text)
	p.bw.WriteString(prefix)
	p.bw.WriteString(text)
	p.bw.WriteString("\n\n")
}
