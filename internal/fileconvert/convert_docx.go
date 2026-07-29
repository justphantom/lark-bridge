package fileconvert

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// docxCtxCheckInterval is how often (in paragraphs) the streaming parser
// polls ctx. Pure-Go parsing cannot hang a process the way a pandoc
// subprocess could, but a hostile docx (deep nesting, millions of empty
// paragraphs) could still pin the dispatcher goroutine without a budget.
const docxCtxCheckInterval = 64

// maxListLevel caps the list indentation level (OOXML allows 0-8); a hostile
// ilvl attribute cannot become an unbounded strings.Repeat indent.
const maxListLevel = 8

// maxGridSpan caps a table cell's column span; a hostile gridSpan cannot pad
// a row with millions of empty strings.
const maxGridSpan = 64

// convertDocx renders a .docx file into GFM Markdown in-process (L1+ scope:
// headings, inline emphasis, lists, tables, hyperlinks, footnotes —
// docx-extract-design.md §2). It replaces the former pandoc subprocess; no
// external binary is required. Malformed sub-parts degrade per-part (missing
// styles.xml → no headings; missing numbering.xml → bullet fallback) rather
// than failing the whole file; only a broken word/document.xml itself is
// fatal.
func (c *Converter) convertDocx(ctx context.Context, srcPath, dstPath string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	base := filepath.Base(srcPath)

	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return fmt.Errorf("fileconvert: open docx %s: %w", base, err)
	}
	defer func() { _ = zr.Close() }()
	parts := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		parts[zf.Name] = zf
	}

	// Pre-pass (docx-extract-design.md §2.2): build the indexes the streaming
	// pass consults. Every part is optional; a read/parse failure degrades
	// that feature instead of failing the conversion.
	links := map[string]string{}
	for id, rel := range relsMap(parts, "word/_rels/document.xml.rels") {
		if rel.External && strings.Contains(rel.Type, "/hyperlink") {
			links[id] = rel.Target
		}
	}
	styles := parseStyles(readPartOrNil(parts, "word/styles.xml"))
	numbering := parseNumbering(readPartOrNil(parts, "word/numbering.xml"))
	footnotes := parseFootnotes(readPartOrNil(parts, "word/footnotes.xml"))

	docPart := parts["word/document.xml"]
	if docPart == nil {
		return fmt.Errorf("fileconvert: docx %s missing word/document.xml", base)
	}
	data, err := readZipPart(docPart)
	if err != nil {
		return fmt.Errorf("fileconvert: read document.xml of %s: %w", base, err)
	}

	bw := &bytes.Buffer{}
	p := &docParser{
		bw:        bw,
		styles:    styles,
		numbering: numbering,
		links:     links,
		footnotes: footnotes,
		list:      newListState(),
		fnSeen:    map[string]bool{},
	}
	if perr := p.safeRun(ctx, data); perr != nil {
		return fmt.Errorf("fileconvert: parse docx %s: %w", base, perr)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fileconvert: docx %s conversion budget exhausted: %w", base, err)
	}
	p.emitFootnotes()

	// The missing-numbering-definition aggregate belongs at the document
	// head (§3.2), so it is prepended after the body is complete.
	var out bytes.Buffer
	if p.missingNumPr > 0 {
		fmt.Fprintf(&out, "<!-- %d 个列表段落缺少编号定义，已降级为无序列表 -->\n\n", p.missingNumPr)
	}
	out.Write(bw.Bytes())

	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileconvert: create docx dst: %w", err)
	}
	if _, err := out.WriteTo(dst); err != nil {
		_ = dst.Close()
		_ = os.Remove(dstPath)
		return fmt.Errorf("fileconvert: write docx dst: %w", err)
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("fileconvert: close docx dst: %w", err)
	}
	if c.log != nil {
		c.log.Debug("fileconvert: docx converted",
			"src", base, "dst", filepath.Base(dstPath))
	}
	return nil
}

// readPartOrNil returns the part's bytes, or nil when absent/unreadable —
// callers treat both as "feature unavailable" and degrade.
func readPartOrNil(parts map[string]*zip.File, name string) []byte {
	zf := parts[name]
	if zf == nil {
		return nil
	}
	data, err := readZipPart(zf)
	if err != nil {
		return nil
	}
	return data
}

// docParser is a streaming token-based extractor for word/document.xml,
// built on the slideParser pattern (convert_pptx.go): it tracks just enough
// context — current paragraph segments, run formatting, table cell state,
// list counters — to emit GFM without modelling the full w:document tree.
type docParser struct {
	bw        *bytes.Buffer
	styles    *styleIndex
	numbering *numberingIndex
	links     map[string]string
	footnotes map[string]string
	list      *listState

	// paragraph state (reset on each <w:p>)
	inP      bool
	skipP    bool // inside a text box — content not extracted (§2.8)
	segs     []segment
	pStyle   string
	outline  string // direct w:outlineLvl override in pPr (rare but legal)
	numID    string
	ilvl     int
	hasNumPr bool
	pics     int // media inside this paragraph, placeholder at paragraph end
	charts   int
	diags    int
	oles     int
	txbx     int
	// picsPending marks an open <w:drawing> not yet classified by its
	// graphicData: tentative picture unless chart/diagram/ole claims it.
	picsPending int

	// run state
	inR     bool
	skipRun bool // w:vanish hidden text (decision Q10)
	fBold   bool
	fItalic bool
	fStrike bool
	fCode   bool
	inT     bool
	inInstr bool // inside w:instrText — field code, never content

	// hyperlink state; runs inherit the active URL ("" = none)
	linkURL string

	// mc:AlternateContent fallback suppression — the Choice branch carries
	// the drawing, the Fallback a VML duplicate; processing both would
	// double-count every image in MS Office exports.
	inFallback bool

	// text-box suppression: paragraphs/tables inside w:txbxContent are
	// skipped wholesale (§2.8), only counted for the placeholder.
	txbxDepth    int
	txbxTblDepth int

	// table state (tblDepth>1 = nested table being flattened, decision §2.6)
	tblDepth   int
	rows       [][]string
	curRow     []string
	curCell    strings.Builder
	inTc       bool
	cellHasTxt bool
	gridSpan   int
	vMergeCont bool
	nestedTbl  bool
	tblPics    int
	tblCharts  int

	// footnote refs in first-use order
	fnRefs []string
	fnSeen map[string]bool

	missingNumPr int
	paraCount    int
	prevList     bool // previous emitted block was a list item (blank-line control)
	stop         bool // ctx budget exhausted mid-stream
}

// safeRun runs the token loop with a recover guard so a parser bug on a
// hostile document surfaces as an error instead of crashing the process.
func (p *docParser) safeRun(ctx context.Context, data []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parser panic: %v", r)
		}
	}()
	p.run(ctx, data)
	return nil
}

func (p *docParser) run(ctx context.Context, data []byte) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil || p.stop {
			return
		}
		switch t := tok.(type) {
		case xml.StartElement:
			p.start(t)
		case xml.EndElement:
			p.end(t, ctx)
		case xml.CharData:
			p.charData(t)
		}
	}
}

func (p *docParser) start(se xml.StartElement) {
	if p.inFallback {
		return
	}
	if se.Name.Space == nsA && se.Name.Local == "graphicData" {
		// graphicData names the embedded object — chart / diagram / ole —
		// classified immediately; a drawing whose graphicData is none of
		// these is a plain picture, counted at </w:drawing>.
		for _, a := range se.Attr {
			if a.Name.Local != "uri" {
				continue
			}
			switch {
			case strings.Contains(a.Value, "/chart"):
				p.charts++
				p.picsPending = 0
			case strings.Contains(a.Value, "/diagram"):
				p.diags++
				p.picsPending = 0
			case strings.Contains(a.Value, "/oleObject"):
				p.oles++
				p.picsPending = 0
			}
		}
		return
	}
	switch se.Name.Local {
	case "AlternateContent":
		// Local-only match: the mc namespace prefix varies by producer.
	case "Fallback":
		p.inFallback = true
		return
	}
	if se.Name.Space != nsW {
		return
	}
	switch se.Name.Local {
	case "p":
		p.inP = true
		p.skipP = p.txbxDepth > 0
		p.segs = nil
		p.pStyle, p.outline, p.numID, p.ilvl, p.hasNumPr = "", "", "", 0, false
		p.pics, p.charts, p.diags, p.oles, p.txbx = 0, 0, 0, 0, 0
		p.picsPending = 0
	case "pStyle":
		if p.inP {
			p.pStyle = attrW(se, "val")
		}
	case "outlineLvl":
		if p.inP {
			p.outline = attrW(se, "val")
		}
	case "numPr":
		if p.inP {
			p.hasNumPr = true
		}
	case "numId":
		if p.inP && p.hasNumPr {
			p.numID = attrW(se, "val")
		}
	case "ilvl":
		if p.inP && p.hasNumPr {
			// Clamp to the OOXML maximum of 9 levels: a hostile ilvl would
			// otherwise become an unbounded strings.Repeat indent.
			p.ilvl = min(atoiSafe(attrW(se, "val")), maxListLevel)
		}
	case "r":
		p.inR = true
		p.skipRun, p.fBold, p.fItalic, p.fStrike, p.fCode = false, false, false, false, false
	case "b":
		if p.inR {
			p.fBold = onOff(se)
		}
	case "i":
		if p.inR {
			p.fItalic = onOff(se)
		}
	case "strike":
		if p.inR {
			p.fStrike = onOff(se)
		}
	case "vanish":
		if p.inR {
			p.skipRun = true
		}
	case "rFonts":
		if p.inR && isMonospace(attrW(se, "ascii"), attrW(se, "hAnsi")) {
			p.fCode = true
		}
	case "rStyle":
		if p.inR && p.styles.isCodeStyle(attrW(se, "val")) {
			p.fCode = true
		}
	case "t":
		p.inT = true
	case "instrText":
		p.inInstr = true
	case "tab":
		p.appendSeg(segment{text: "\t"})
	case "br":
		if attrW(se, "type") != "page" {
			p.appendSeg(segment{text: "<br>"})
		}
	case "cr":
		p.appendSeg(segment{text: "<br>"})
	case "noBreakHyphen":
		p.appendSeg(segment{text: "-"})
	case "footnoteReference":
		id := attrW(se, "id")
		if _, ok := p.footnotes[id]; ok {
			p.appendSeg(segment{text: "[" + id + "]"})
			if !p.fnSeen[id] {
				p.fnSeen[id] = true
				p.fnRefs = append(p.fnRefs, id)
			}
		}
	case "hyperlink":
		p.linkURL = ""
		for _, a := range se.Attr {
			if a.Name.Local == "id" && a.Name.Space == ooRelsNS {
				p.linkURL = p.links[a.Value]
			}
		}
	case "drawing":
		p.picsPending = 1 // tentative; graphicData may reclassify (see start)
	case "object":
		p.oles++
	case "pict":
		p.pics++
	case "txbxContent":
		p.txbxDepth++
		p.txbx++
	case "tbl":
		if p.txbxDepth > 0 {
			p.txbxTblDepth++
			return
		}
		p.tblDepth++
		switch p.tblDepth {
		case 1:
			p.closeList()
			p.rows = nil
			p.nestedTbl = false
			p.tblPics, p.tblCharts = 0, 0
		case 2:
			p.nestedTbl = true
		}
	case "tr":
		if p.tblDepth == 1 {
			p.curRow = nil
		}
	case "tc":
		if p.tblDepth == 1 {
			p.inTc = true
			p.curCell.Reset()
			p.cellHasTxt = false
			p.gridSpan = 1
			p.vMergeCont = false
		}
	case "gridSpan":
		if p.inTc {
			if n := atoiSafe(attrW(se, "val")); n > 1 {
				// Clamp: a hostile gridSpan would otherwise pad the row with
				// millions of empty strings (memory-amplification DoS).
				p.gridSpan = min(n, maxGridSpan)
			}
		}
	case "vMerge":
		if p.inTc {
			if v := attrW(se, "val"); v == "" || v == "continue" {
				p.vMergeCont = true
			}
		}
	}
}

func (p *docParser) end(ee xml.EndElement, ctx context.Context) {
	if ee.Name.Local == "Fallback" {
		p.inFallback = false
		return
	}
	if p.inFallback {
		return
	}
	if ee.Name.Space != nsW {
		return
	}
	switch ee.Name.Local {
	case "p":
		p.inP = false
		p.paraCount++
		if p.paraCount%docxCtxCheckInterval == 0 && ctx.Err() != nil {
			p.stop = true
			return
		}
		if p.skipP {
			p.skipP = false
			return
		}
		text := renderSegments(p.segs)
		if p.inTc {
			if text != "" {
				if p.cellHasTxt {
					p.curCell.WriteString("<br>")
				}
				p.curCell.WriteString(text)
				p.cellHasTxt = true
			}
			// Media inside cells cannot carry a placeholder line without
			// breaking the table; aggregate and annotate after the table.
			p.tblPics += p.pics
			p.tblCharts += p.charts
			return
		}
		p.emitBodyParagraph(text)
	case "t":
		p.inT = false
	case "instrText":
		p.inInstr = false
	case "r":
		p.inR = false
	case "hyperlink":
		p.linkURL = ""
	case "drawing":
		// A drawing whose graphicData matched nothing is a plain picture;
		// chart/diagram/ole hits already zeroed picsPending at the start tag.
		p.pics += p.picsPending
		p.picsPending = 0
	case "txbxContent":
		p.txbxDepth--
	case "tc":
		if p.tblDepth == 1 {
			content := p.curCell.String()
			if p.vMergeCont {
				content = "" // decision Q4: merged region keeps top-left value only
			}
			p.curRow = append(p.curRow, content)
			for i := 1; i < p.gridSpan; i++ {
				p.curRow = append(p.curRow, "")
			}
			p.inTc = false
		}
	case "tr":
		if p.tblDepth == 1 && len(p.curRow) > 0 {
			p.rows = append(p.rows, p.curRow)
			p.curRow = nil
		}
	case "tbl":
		if p.txbxTblDepth > 0 {
			p.txbxTblDepth--
			return
		}
		if p.tblDepth > 1 {
			p.tblDepth-- // nested table: content already flowed into the cell
			return
		}
		p.tblDepth = 0
		p.emitTable()
	}
}

func (p *docParser) charData(cd xml.CharData) {
	if p.inFallback || !p.inT || p.skipRun || p.inInstr {
		return
	}
	p.appendSeg(segment{
		text:   string(cd),
		bold:   p.fBold,
		italic: p.fItalic,
		strike: p.fStrike,
		code:   p.fCode,
		link:   p.linkURL,
	})
}

// appendSeg adds one segment, coalescing with the previous one when the
// format matches — runs split words arbitrarily, so same-format neighbours
// must join seamlessly to avoid "**wo****rd**" marker soup. Runs inside a
// vanish run or field instruction contribute nothing.
func (p *docParser) appendSeg(s segment) {
	if !p.inP || p.skipP || p.skipRun || p.inInstr || s.text == "" {
		return
	}
	if n := len(p.segs); n > 0 {
		last := &p.segs[n-1]
		if last.bold == s.bold && last.italic == s.italic &&
			last.strike == s.strike && last.code == s.code && last.link == s.link {
			last.text += s.text
			return
		}
	}
	p.segs = append(p.segs, s)
}

// atoiSafe parses a non-negative int attribute, returning 0 on any garbage.
// An overflowing digit string also yields 0 (treated as garbage) — wrapping
// into a negative would turn a hostile attribute into a slice index or a
// strings.Repeat count panic.
func atoiSafe(s string) int {
	n := 0
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		d := int(s[i] - '0')
		if n > (math.MaxInt-d)/10 {
			return 0
		}
		n = n*10 + d
	}
	return n
}
