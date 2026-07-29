package fileconvert

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

// segment is one formatted run inside a paragraph: text plus the inline
// markers active over it. Adjacent same-format segments are coalesced at
// append time (docParser.appendSeg), so renderSegments can wrap markers
// without further merging.
type segment struct {
	text         string
	bold, italic bool
	strike, code bool
	link         string // resolved URL, "" = none
}

// renderSegments converts a paragraph's segments into GFM inline text.
// Code spans win over emphasis (a backtick span showing literal ** is more
// useful than broken nesting); hyperlinks wrap whatever the inner markers
// produced.
func renderSegments(segs []segment) string {
	var b strings.Builder
	for _, s := range segs {
		text := s.text
		if text == "" {
			continue
		}
		switch {
		case s.code:
			text = "`" + text + "`"
		case s.bold && s.italic:
			text = wrapEmph(text, "***")
		case s.bold:
			text = wrapEmph(text, "**")
		case s.italic:
			text = wrapEmph(text, "*")
		}
		if s.strike && !s.code {
			text = wrapEmph(text, "~~")
		}
		if s.link != "" {
			text = "[" + text + "](" + s.link + ")"
		}
		b.WriteString(text)
	}
	return b.String()
}

// wrapEmph puts marker around the trimmed core, leaving surrounding spaces
// outside — "** bold **" does not render as emphasis in GFM, "**bold** "
// does.
func wrapEmph(s, marker string) string {
	trimmed := strings.Trim(s, " ")
	if trimmed == "" {
		return s
	}
	lead := s[:len(s)-len(strings.TrimLeft(s, " "))]
	trail := s[len(strings.TrimRight(s, " ")):]
	return lead + marker + trimmed + marker + trail
}

// plainSegments flattens segments to raw text for fenced code blocks, where
// inline markers must not be resolved (code shows literally). Soft-break
// sentinels become real newlines.
func plainSegments(segs []segment) string {
	var b strings.Builder
	for _, s := range segs {
		if s.text == "<br>" {
			b.WriteByte('\n')
		} else {
			b.WriteString(s.text)
		}
	}
	return b.String()
}

// emitBodyParagraph writes one non-table paragraph with the right block
// prefix: fenced code for code styles, "#"×n for headings, list marker for
// numbered/bulleted paragraphs, bare text otherwise. Media counters
// accumulated inside the paragraph become placeholder lines after it.
func (p *docParser) emitBodyParagraph(text string) {
	hasMedia := p.pics > 0 || p.charts > 0 || p.diags > 0 || p.oles > 0 || p.txbx > 0
	if text == "" && !hasMedia {
		return
	}

	// Code paragraph (decision Q7): raw text into a fenced block. The fence
	// length adapts to the content so a ``` inside the code cannot break out.
	if text != "" && p.styles.isCodeStyle(p.pStyle) {
		p.closeList()
		raw := plainSegments(p.segs)
		fence := gfmFence(raw)
		p.bw.WriteString(fence + "\n")
		p.bw.WriteString(raw)
		if !strings.HasSuffix(raw, "\n") {
			p.bw.WriteByte('\n')
		}
		p.bw.WriteString(fence + "\n\n")
		return
	}

	heading := p.styles.headingLevel(p.pStyle, p.outline)

	// List resolution order (§2.4): direct numPr on the paragraph →
	// styles.xml style-linked numPr → numbering.xml reverse pStyle link.
	numID, ilvl, isList := p.numID, p.ilvl, p.hasNumPr
	if !isList && p.pStyle != "" {
		if id, lv, ok := p.styles.styleList(p.pStyle); ok {
			numID, ilvl, isList = id, lv, true
		} else if l, ok := p.numbering.styleLink(p.pStyle); ok {
			numID, ilvl, isList = l.numID, l.ilvl, true
		}
	}

	switch {
	case isList && text != "" && heading > 0:
		// Numbered heading ("# 1. 概述"): heading prefix plus the counter,
		// without list indentation.
		marker, ok := p.list.marker(p.numbering, numID, ilvl)
		if !ok {
			marker = "- "
			p.missingNumPr++
		}
		p.closeList()
		p.bw.WriteString(strings.Repeat("#", heading) + " " + strings.TrimLeft(marker, " ") + text + "\n\n")
	case isList && text != "":
		marker, ok := p.list.marker(p.numbering, numID, ilvl)
		if !ok {
			marker = strings.Repeat("  ", min(max(ilvl, 0), maxListLevel)) + "- "
			p.missingNumPr++
		}
		p.bw.WriteString(marker + escapeLeading(text) + "\n")
		p.prevList = true
	case text != "":
		p.closeList()
		if heading > 0 {
			p.bw.WriteString(strings.Repeat("#", heading) + " " + text + "\n\n")
		} else {
			p.bw.WriteString(escapeLeading(text) + "\n\n")
		}
	}
	p.emitMediaPlaceholders()
}

// emitMediaPlaceholders writes the paragraph-position placeholder lines for
// media encountered inside the paragraph (decision Q3 + §3.2). Empty
// paragraphs that carried only media still get their placeholders.
func (p *docParser) emitMediaPlaceholders() {
	if p.pics > 0 {
		p.writePlaceholder("<!-- 此处有 " + itoa(p.pics) + " 张图片，未提取 -->")
	}
	if p.charts > 0 {
		p.writePlaceholder("<!-- 含 " + itoa(p.charts) + " 个图表（chart），未提取 -->")
	}
	if p.diags > 0 {
		p.writePlaceholder("<!-- 含 SmartArt，未提取 -->")
	}
	if p.oles > 0 {
		p.writePlaceholder("<!-- 含嵌入对象（OLE），未提取 -->")
	}
	if p.txbx > 0 {
		p.writePlaceholder("<!-- 此处有文本框，未提取 -->")
	}
}

func (p *docParser) writePlaceholder(s string) {
	p.closeList()
	p.bw.WriteString(s + "\n\n")
}

// emitTable flushes the accumulated rows through the shared renderTable
// (GFM pipes, or fenced CSV beyond gfmMaxColumns) and appends the
// nested-table / in-table-media annotations. The first row is the GFM
// header regardless of tblHeader (GFM forces a header row; pandoc does the
// same).
func (p *docParser) emitTable() {
	if len(p.rows) > 0 {
		p.bw.WriteString(renderTable(p.rows))
		p.bw.WriteByte('\n')
	}
	if p.nestedTbl {
		p.bw.WriteString("<!-- 含嵌套表格，已平铺 -->\n\n")
	}
	if p.tblPics > 0 {
		p.bw.WriteString("<!-- 表格内含 " + itoa(p.tblPics) + " 张图片，未提取 -->\n\n")
	}
	if p.tblCharts > 0 {
		p.bw.WriteString("<!-- 表格内含 " + itoa(p.tblCharts) + " 个图表（chart），未提取 -->\n\n")
	}
	p.rows = nil
}

// emitFootnotes appends the footnote section for refs used in the body
// (decision Q6), in first-use order.
func (p *docParser) emitFootnotes() {
	p.closeList()
	if len(p.fnRefs) == 0 {
		return
	}
	p.bw.WriteString("---\n\n## 脚注\n\n")
	for _, id := range p.fnRefs {
		p.bw.WriteString("[" + id + "] " + p.footnotes[id] + "\n\n")
	}
}

// closeList terminates an open list block: GFM needs a blank line before
// the following block or the block is swallowed into the last list item.
func (p *docParser) closeList() {
	if p.prevList {
		p.bw.WriteByte('\n')
		p.prevList = false
	}
}

// escapeLeading backslash-escapes text that GFM would misread as a block
// construct when it opens a plain paragraph or list item: "#"/">" headings
// and quotes, "- "/"+" bullets, "1. " ordered items, "|" table rows, and
// ``` / ~~~ code fences.
func escapeLeading(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '#', '>', '|':
		return `\` + s
	case '-', '+':
		if len(s) > 1 && s[1] == ' ' {
			return `\` + s
		}
		return s
	case '`':
		if strings.HasPrefix(s, "```") {
			return `\` + s
		}
		return s
	case '~':
		if strings.HasPrefix(s, "~~~") {
			return `\` + s
		}
		return s
	}
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s)-1 && (s[i] == '.' || s[i] == ')') && s[i+1] == ' ' {
		return s[:i] + `\` + s[i:]
	}
	return s
}

// monospaceFonts is the decision-Q7 font whitelist for inline code
// detection (w:rFonts ascii/hAnsi, lowercased).
var monospaceFonts = map[string]bool{
	"consolas":           true,
	"courier new":        true,
	"courier":            true,
	"menlo":              true,
	"monaco":             true,
	"jetbrains mono":     true,
	"lucida console":     true,
	"dejavu sans mono":   true,
	"source code pro":    true,
	"cascadia mono":      true,
	"cascadia code":      true,
	"fira code":          true,
	"ubuntu mono":        true,
	"roboto mono":        true,
	"noto sans mono":     true,
	"noto sans mono cjk": true,
	"sarasa mono sc":     true,
}

// isMonospace reports whether any of the given font names is in the
// monospace whitelist.
func isMonospace(names ...string) bool {
	for _, n := range names {
		if monospaceFonts[strings.ToLower(n)] {
			return true
		}
	}
	return false
}

// onOff interprets an OOXML on/off element: present with no w:val means ON;
// explicit "0"/"false"/"off" mean OFF (a run can disable inherited bold).
func onOff(se xml.StartElement) bool {
	v := attrW(se, "val")
	return v != "0" && v != "false" && v != "off"
}

// parseFootnotes digests word/footnotes.xml into id → plain text. Footnote
// bodies reuse normal paragraph XML but are flattened to unformatted text
// (§2.5); the built-in separator/continuation entries (w:type set, ids ≤ 0)
// are skipped. nil/garbage input yields an empty map.
func parseFootnotes(data []byte) map[string]string {
	out := map[string]string{}
	if len(data) == 0 {
		return out
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var curID string
	var buf strings.Builder
	inFn := false
	inT := false
	firstPara := true
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != nsW {
				continue
			}
			switch t.Name.Local {
			case "footnote":
				id := attrW(t, "id")
				n, _ := strconv.Atoi(id)
				if id != "" && attrW(t, "type") == "" && n > 0 {
					inFn, curID = true, id
					buf.Reset()
					firstPara = true
				}
			case "p":
				if inFn {
					if firstPara {
						firstPara = false
					} else {
						buf.WriteString(" ")
					}
				}
			case "t":
				if inFn {
					inT = true
				}
			}
		case xml.EndElement:
			if t.Name.Space != nsW {
				continue
			}
			switch t.Name.Local {
			case "t":
				inT = false
			case "footnote":
				if inFn {
					if s := strings.TrimSpace(buf.String()); s != "" {
						out[curID] = s
					}
					inFn = false
				}
			}
		case xml.CharData:
			if inFn && inT {
				buf.Write(t)
			}
		}
	}
}

// itoa is strconv.Itoa kept local so emit code reads without stutter.
func itoa(n int) string { return strconv.Itoa(n) }
