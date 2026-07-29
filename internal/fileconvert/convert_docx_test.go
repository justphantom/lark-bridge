package fileconvert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docx fixture builders. A docx is a zip of OOXML parts, so fixtures are
// authored inline as XML — reproducible and dependency-free, no pandoc and
// no MS Word required in CI (docx-extract-design.md §5.1).

const wDecl = `xmlns:w="` + nsW + `" xmlns:r="` + ooRelsNS + `" xmlns:a="` + nsA + `"`

func docxDoc(body string) string {
	return `<w:document ` + wDecl + `><w:body>` + body + `</w:body></w:document>`
}

func docxPara(text string) string {
	return `<w:p><w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

// docxStyledPara emits a paragraph carrying a pStyle and optional extra pPr
// XML (outlineLvl / numPr).
func docxStyledPara(style, extraPPr, text string) string {
	pPr := `<w:pPr><w:pStyle w:val="` + style + `"/>` + extraPPr + `</w:pPr>`
	return `<w:p>` + pPr + `<w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func docxListPara(numID string, ilvl int, text string) string {
	return `<w:p><w:pPr><w:numPr><w:ilvl w:val="` + itoa(ilvl) + `"/><w:numId w:val="` + numID + `"/></w:numPr></w:pPr><w:r><w:t>` + text + `</w:t></w:r></w:p>`
}

func docxStyles(entries ...string) string {
	return `<w:styles ` + wDecl + `>` + strings.Join(entries, "") + `</w:styles>`
}

func styleEntry(id, name, inner string) string {
	return `<w:style w:type="paragraph" w:styleId="` + id + `"><w:name w:val="` + name + `"/>` + inner + `</w:style>`
}

// docxNumbering wraps abstractNum + num definitions.
func docxNumbering(inner string) string {
	return `<w:numbering ` + wDecl + `>` + inner + `</w:numbering>`
}

// absNum emits one abstractNum with the given per-level (ilvl, fmt) defs.
func absNum(id string, lvls ...[2]string) string {
	var b strings.Builder
	b.WriteString(`<w:abstractNum w:abstractNumId="` + id + `">`)
	for _, l := range lvls {
		b.WriteString(`<w:lvl w:ilvl="` + l[0] + `"><w:start w:val="1"/><w:numFmt w:val="` + l[1] + `"/><w:lvlText w:val="%1."/></w:lvl>`)
	}
	b.WriteString(`</w:abstractNum>`)
	return b.String()
}

func numRef(numID, absID string) string {
	return `<w:num w:numId="` + numID + `"><w:abstractNumId w:val="` + absID + `"/></w:num>`
}

// relXMLExt is relXML plus TargetMode="External" (hyperlinks).
func relXMLExt(id, typ, target string) string {
	return `<Relationship Id="` + id + `" Type="` + typ + `" Target="` + target + `" TargetMode="External"/>`
}

const relHyperlink = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"

// writeDocx assembles a fixture docx on disk. files must include
// word/document.xml.
func writeDocx(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	writeZip(t, src, files)
	return src
}

func convertDocxTo(t *testing.T, c *Converter, src string) string {
	t.Helper()
	dst := strings.TrimSuffix(src, ".docx") + ".md"
	if err := c.Convert(context.Background(), src, dst); err != nil {
		t.Fatalf("Convert docx: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	return string(body)
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(body, w) {
			t.Errorf("missing %q:\n%s", w, body)
		}
	}
}

func mustNotContain(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(body, u) {
			t.Errorf("unexpected %q:\n%s", u, body)
		}
	}
}

func TestConvertDocx_HeadingOutlineLvl(t *testing.T) {
	src := writeDocx(t, "h.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxStyledPara("H1", "", "一级") +
				docxStyledPara("H2", "", "二级") +
				docxStyledPara("H3", "", "三级")),
		"word/styles.xml": docxStyles(
			styleEntry("H1", "heading 1", `<w:pPr><w:outlineLvl w:val="0"/></w:pPr>`),
			styleEntry("H2", "heading 2", `<w:pPr><w:outlineLvl w:val="1"/></w:pPr>`),
			styleEntry("H3", "heading 3", `<w:pPr><w:outlineLvl w:val="2"/></w:pPr>`)),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "# 一级\n", "## 二级\n", "### 三级\n")
}

func TestConvertDocx_HeadingBasedOnChain(t *testing.T) {
	// Sub carries no outlineLvl; the basedOn chain must resolve to the
	// parent's level.
	src := writeDocx(t, "b.docx", map[string]string{
		"word/document.xml": docxDoc(docxStyledPara("Sub", "", "链式标题")),
		"word/styles.xml": docxStyles(
			styleEntry("Sub", "custom sub", `<w:basedOn w:val="Base"/>`),
			styleEntry("Base", "base", `<w:pPr><w:outlineLvl w:val="1"/></w:pPr>`)),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "## 链式标题\n")
}

func TestConvertDocx_HeadingNameFallback(t *testing.T) {
	// No outlineLvl anywhere (some WPS exports): fall back to style names,
	// both English and Chinese.
	src := writeDocx(t, "nf.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxStyledPara("A", "", "英文回退") + docxStyledPara("B", "", "中文回退")),
		"word/styles.xml": docxStyles(
			styleEntry("A", "heading 2", ""),
			styleEntry("B", "标题 3", "")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "## 英文回退\n", "### 中文回退\n")
}

func TestConvertDocx_OutlineLvl9IsBody(t *testing.T) {
	src := writeDocx(t, "nine.docx", map[string]string{
		"word/document.xml": docxDoc(docxStyledPara("Body", "", "正文段落")),
		"word/styles.xml": docxStyles(
			styleEntry("Body", "body text", `<w:pPr><w:outlineLvl w:val="9"/></w:pPr>`)),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "正文段落\n")
	mustNotContain(t, body, "# 正文段落")
}

func TestConvertDocx_DirectOutlineLvlBeatsStyle(t *testing.T) {
	// A direct outlineLvl on the paragraph overrides the style's level.
	src := writeDocx(t, "direct.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxStyledPara("H3", `<w:outlineLvl w:val="0"/>`, "直改一级")),
		"word/styles.xml": docxStyles(
			styleEntry("H3", "heading 3", `<w:pPr><w:outlineLvl w:val="2"/></w:pPr>`)),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "# 直改一级\n")
	mustNotContain(t, body, "### 直改一级")
}

func TestConvertDocx_InlineFormats(t *testing.T) {
	src := writeDocx(t, "fmt.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p>` +
			`<w:r><w:rPr><w:b/></w:rPr><w:t>粗体</w:t></w:r>` +
			`<w:r><w:t>和</w:t></w:r>` +
			`<w:r><w:rPr><w:i/></w:rPr><w:t>斜体</w:t></w:r>` +
			`<w:r><w:rPr><w:strike/></w:rPr><w:t>删除</w:t></w:r>` +
			`<w:r><w:rPr><w:b/><w:i/></w:rPr><w:t>粗斜</w:t></w:r>` +
			`<w:r><w:rPr><w:rFonts w:ascii="Consolas"/></w:rPr><w:t>代码</w:t></w:r>` +
			`</w:p>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "**粗体**", "*斜体*", "~~删除~~", "***粗斜***", "`代码`")
}

func TestConvertDocx_AdjacentSameFormatCoalesces(t *testing.T) {
	// Two consecutive bold runs form one marker span, not "**AB****CD**".
	src := writeDocx(t, "coal.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p>` +
			`<w:r><w:rPr><w:b/></w:rPr><w:t>AB</w:t></w:r>` +
			`<w:r><w:rPr><w:b/></w:rPr><w:t>CD</w:t></w:r>` +
			`</w:p>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "**ABCD**")
	mustNotContain(t, body, "**AB****CD**")
}

func TestConvertDocx_HyperlinkExternal(t *testing.T) {
	src := writeDocx(t, "link.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p><w:hyperlink r:id="rId9">` +
			`<w:r><w:t>财报原文</w:t></w:r></w:hyperlink></w:p>`),
		"word/_rels/document.xml.rels": `<Relationships ` + pkgRelsNS + `>` +
			relXMLExt("rId9", relHyperlink, "https://example.com/fy26") +
			`</Relationships>`,
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "[财报原文](https://example.com/fy26)")
}

func TestConvertDocx_HyperlinkAnchorTextOnly(t *testing.T) {
	src := writeDocx(t, "anchor.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p><w:hyperlink w:anchor="sec1">` +
			`<w:r><w:t>内部跳转</w:t></w:r></w:hyperlink></w:p>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "内部跳转")
	mustNotContain(t, body, "[内部跳转](")
}

func TestConvertDocx_BulletAndDecimalLists(t *testing.T) {
	src := writeDocx(t, "list.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxListPara("5", 0, "甲") + docxListPara("5", 0, "乙") +
				docxListPara("7", 0, "第一") + docxListPara("7", 0, "第二")),
		"word/numbering.xml": docxNumbering(
			absNum("0", [2]string{"0", "bullet"}) +
				absNum("1", [2]string{"0", "decimal"}) +
				numRef("5", "0") + numRef("7", "1")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "- 甲\n", "- 乙\n", "1. 第一\n", "2. 第二\n")
}

func TestConvertDocx_ListIndentAndRestart(t *testing.T) {
	// Deeper counters restart when a shallower item appears.
	src := writeDocx(t, "restart.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxListPara("5", 0, "a") +
				docxListPara("5", 1, "x") + docxListPara("5", 1, "y") +
				docxListPara("5", 0, "b") +
				docxListPara("5", 1, "z")),
		"word/numbering.xml": docxNumbering(
			absNum("0", [2]string{"0", "decimal"}, [2]string{"1", "decimal"}) +
				numRef("5", "0")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "1. a\n", "  1. x\n", "  2. y\n", "2. b\n", "  1. z\n")
}

func TestConvertDocx_LetterFmtDegradesToDecimal(t *testing.T) {
	// decision Q5: lowerLetter renders as a decimal counter.
	src := writeDocx(t, "letter.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxListPara("5", 0, "x") + docxListPara("5", 0, "y")),
		"word/numbering.xml": docxNumbering(
			absNum("0", [2]string{"0", "lowerLetter"}) + numRef("5", "0")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "1. x\n", "2. y\n")
}

func TestConvertDocx_ListMissingDefinition(t *testing.T) {
	// numPr references a numId numbering.xml does not define → bullet
	// fallback plus the document-head aggregate placeholder.
	src := writeDocx(t, "nodef.docx", map[string]string{
		"word/document.xml":  docxDoc(docxListPara("99", 0, "孤儿") + docxListPara("99", 0, "孤儿二")),
		"word/numbering.xml": docxNumbering(absNum("0", [2]string{"0", "bullet"}) + numRef("5", "0")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "- 孤儿\n", "<!-- 2 个列表段落缺少编号定义，已降级为无序列表 -->")
	if strings.Index(body, "<!--") > strings.Index(body, "- 孤儿") {
		t.Errorf("aggregate placeholder should lead the document:\n%s", body)
	}
}

func TestConvertDocx_StyleDrivenNumberedHeading(t *testing.T) {
	// numbering.xml's reverse pStyle link (Word's numbered headings): the
	// paragraph carries only pStyle, no numPr anywhere in styles.xml.
	src := writeDocx(t, "snh.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxStyledPara("Heading1", "", "概述") + docxStyledPara("Heading1", "", "结论")),
		"word/styles.xml": docxStyles(
			styleEntry("Heading1", "heading 1", `<w:pPr><w:outlineLvl w:val="0"/></w:pPr>`)),
		"word/numbering.xml": docxNumbering(
			`<w:abstractNum w:abstractNumId="0"><w:lvl w:ilvl="0">` +
				`<w:start w:val="1"/><w:numFmt w:val="decimal"/>` +
				`<w:pStyle w:val="Heading1"/></w:lvl></w:abstractNum>` +
				numRef("6", "0")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "# 1. 概述\n", "# 2. 结论\n")
}

func TestConvertDocx_TableFirstRowHeader(t *testing.T) {
	src := writeDocx(t, "tbl.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:tbl>` +
			`<w:tr><w:tc><w:p><w:r><w:t>区域</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>销售额</w:t></w:r></w:p></w:tc></w:tr>` +
			`<w:tr><w:tc><w:p><w:r><w:t>华东</w:t></w:r></w:p></w:tc><w:tc><w:p><w:r><w:t>1.2M</w:t></w:r></w:p></w:tc></w:tr>` +
			`</w:tbl>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "| 区域 | 销售额 |", "| 华东 | 1.2M |")
}

func TestConvertDocx_TableGridSpanAndVMerge(t *testing.T) {
	// decision Q4: merged regions keep only the top-left value.
	src := writeDocx(t, "merge.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:tbl>` +
			`<w:tr>` +
			`<w:tc><w:tcPr><w:gridSpan w:val="2"/></w:tcPr><w:p><w:r><w:t>横跨</w:t></w:r></w:p></w:tc>` +
			`<w:tc><w:p><w:r><w:t>尾列</w:t></w:r></w:p></w:tc>` +
			`</w:tr>` +
			`<w:tr>` +
			`<w:tc><w:tcPr><w:vMerge w:val="restart"/></w:tcPr><w:p><w:r><w:t>纵起</w:t></w:r></w:p></w:tc>` +
			`<w:tc><w:p><w:r><w:t>a</w:t></w:r></w:p></w:tc>` +
			`<w:tc><w:p><w:r><w:t>b</w:t></w:r></w:p></w:tc>` +
			`</w:tr>` +
			`<w:tr>` +
			`<w:tc><w:tcPr><w:vMerge/></w:tcPr><w:p><w:r><w:t>应消失</w:t></w:r></w:p></w:tc>` +
			`<w:tc><w:p><w:r><w:t>c</w:t></w:r></w:p></w:tc>` +
			`<w:tc><w:p><w:r><w:t>d</w:t></w:r></w:p></w:tc>` +
			`</w:tr>` +
			`</w:tbl>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "| 横跨 |  | 尾列 |", "| 纵起 | a | b |", "|  | c | d |")
	mustNotContain(t, body, "应消失")
}

func TestConvertDocx_TableNestedFlattened(t *testing.T) {
	src := writeDocx(t, "nest.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:tbl><w:tr><w:tc>` +
			`<w:p><w:r><w:t>外层前</w:t></w:r></w:p>` +
			`<w:tbl><w:tr><w:tc><w:p><w:r><w:t>内层</w:t></w:r></w:p></w:tc></w:tr></w:tbl>` +
			`<w:p><w:r><w:t>外层后</w:t></w:r></w:p>` +
			`</w:tc></w:tr></w:tbl>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "外层前", "内层", "外层后", "<!-- 含嵌套表格，已平铺 -->")
}

func TestConvertDocx_TableWideDegradesToCSV(t *testing.T) {
	// >20 columns switches renderTable to a fenced CSV block.
	var tcs strings.Builder
	for range 21 {
		tcs.WriteString(`<w:tc><w:p><w:r><w:t>c</w:t></w:r></w:p></w:tc>`)
	}
	src := writeDocx(t, "wide.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:tbl><w:tr>` + tcs.String() + `</w:tr></w:tbl>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "```csv")
}

func TestConvertDocx_ImagePlaceholder(t *testing.T) {
	src := writeDocx(t, "img.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p><w:r><w:t>前文</w:t></w:r>` +
			`<w:r><w:drawing><wp:inline><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/picture"/></a:graphic></wp:inline></w:drawing></w:r>` +
			`</w:p>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "前文", "<!-- 此处有 1 张图片，未提取 -->")
}

func TestConvertDocx_ChartAndSmartArtPlaceholders(t *testing.T) {
	src := writeDocx(t, "media.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p>` +
			`<w:r><w:drawing><wp:inline><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/chart"/></a:graphic></wp:inline></w:drawing></w:r>` +
			`<w:r><w:drawing><wp:inline><a:graphic><a:graphicData uri="http://schemas.openxmlformats.org/drawingml/2006/diagram"/></a:graphic></wp:inline></w:drawing></w:r>` +
			`</w:p>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "<!-- 含 1 个图表（chart），未提取 -->", "<!-- 含 SmartArt，未提取 -->")
	mustNotContain(t, body, "图片")
}

func TestConvertDocx_CodeParagraph(t *testing.T) {
	src := writeDocx(t, "code.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxStyledPara("Code", "", "func main() { **不是加粗** }")),
		"word/styles.xml": docxStyles(styleEntry("Code", "HTML Code", "")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "```\nfunc main() { **不是加粗** }\n```")
}

func TestConvertDocx_Footnotes(t *testing.T) {
	src := writeDocx(t, "fn.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p><w:r><w:t>正文</w:t></w:r>` +
			`<w:r><w:footnoteReference w:id="2"/></w:r></w:p>`),
		"word/footnotes.xml": `<w:footnotes ` + wDecl + `>` +
			`<w:footnote w:type="separator" w:id="-1"><w:p><w:r><w:separator/></w:r></w:p></w:footnote>` +
			`<w:footnote w:id="2"><w:p><w:r><w:t>数据来源：财务部</w:t></w:r></w:p></w:footnote>` +
			`</w:footnotes>`,
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "正文[2]", "## 脚注", "[2] 数据来源：财务部")
}

func TestConvertDocx_SdtPassthrough(t *testing.T) {
	src := writeDocx(t, "sdt.docx", map[string]string{
		"word/document.xml": docxDoc(
			`<w:sdt><w:sdtPr/><w:sdtContent>` + docxPara("控件文本") + `</w:sdtContent></w:sdt>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "控件文本")
}

func TestConvertDocx_VanishSkipped(t *testing.T) {
	src := writeDocx(t, "vanish.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p>` +
			`<w:r><w:t>可见</w:t></w:r>` +
			`<w:r><w:rPr><w:vanish/></w:rPr><w:t>隐藏</w:t></w:r>` +
			`</w:p>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "可见")
	mustNotContain(t, body, "隐藏")
}

func TestConvertDocx_InstrTextSkipped(t *testing.T) {
	// TOC field code must not leak into the output; the field result text
	// (between separate and end) is kept.
	src := writeDocx(t, "toc.docx", map[string]string{
		"word/document.xml": docxDoc(`<w:p>` +
			`<w:r><w:fldChar w:fldCharType="begin"/></w:r>` +
			`<w:r><w:instrText> TOC \o "1-3" </w:instrText></w:r>` +
			`<w:r><w:fldChar w:fldCharType="separate"/></w:r>` +
			`<w:r><w:t>目录结果</w:t></w:r>` +
			`<w:r><w:fldChar w:fldCharType="end"/></w:r>` +
			`</w:p>`),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "目录结果")
	mustNotContain(t, body, "TOC")
}

func TestConvertDocx_LeadingCharsEscaped(t *testing.T) {
	src := writeDocx(t, "esc.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxPara("# 不是标题") + docxPara("- 不是列表") + docxPara("1. 不是编号") +
				docxPara("| 不是表格") + docxPara("```") + docxPara("~~~")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, `\# 不是标题`, `\- 不是列表`, `1\. 不是编号`, `\| 不是表格`, "\\```", "\\~~~")
}

func TestConvertDocx_MissingSubPartsDegrade(t *testing.T) {
	// No styles.xml / numbering.xml at all: styled paragraphs become body
	// text, numPr lists become bullets with the aggregate placeholder.
	src := writeDocx(t, "bare.docx", map[string]string{
		"word/document.xml": docxDoc(
			docxStyledPara("Heading1", "", "无样式标题") + docxListPara("5", 0, "无定义")),
	})
	body := convertDocxTo(t, New(Options{}), src)
	mustContain(t, body, "无样式标题\n", "- 无定义\n", "缺少编号定义")
	mustNotContain(t, body, "# 无样式标题")
}

func TestConvertDocx_CorruptFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bad.docx")
	if err := os.WriteFile(src, []byte("not a zip"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := New(Options{}).Convert(context.Background(), src, src+".md"); err == nil {
		t.Errorf("expected error on corrupt docx, got nil")
	}
}

func TestConvertDocx_MissingDocumentPartFails(t *testing.T) {
	src := writeDocx(t, "nodoc.docx", map[string]string{
		"word/styles.xml": docxStyles(),
	})
	if err := New(Options{}).Convert(context.Background(), src, src+".md"); err == nil {
		t.Errorf("expected error when word/document.xml missing, got nil")
	}
}

func TestConvertDocx_CancelledCtxAborts(t *testing.T) {
	// Deterministic budget test: a pre-cancelled ctx plus >64 paragraphs
	// trips the interval check and fails the conversion (no dst written).
	var paras strings.Builder
	for range 100 {
		paras.WriteString(`<w:p/>`)
	}
	src := writeDocx(t, "big.docx", map[string]string{
		"word/document.xml": docxDoc(paras.String()),
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := strings.TrimSuffix(src, ".docx") + ".md"
	if err := New(Options{}).Convert(ctx, src, dst); err == nil {
		t.Errorf("expected error on cancelled ctx, got nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst should not exist after cancelled conversion")
	}
}

func TestHeadingLevel_PriorityOrder(t *testing.T) {
	idx := parseStyles([]byte(docxStyles(
		styleEntry("Child", "heading 9", `<w:basedOn w:val="Mid"/>`),
		styleEntry("Mid", "mid", `<w:basedOn w:val="Root"/>`),
		styleEntry("Root", "root", `<w:pPr><w:outlineLvl w:val="2"/></w:pPr>`),
		styleEntry("Named", "heading 4", ""),
	)))
	if got := idx.headingLevel("Child", ""); got != 3 {
		t.Errorf("basedOn chain: got %d, want 3", got)
	}
	if got := idx.headingLevel("Child", "0"); got != 1 {
		t.Errorf("direct outlineLvl should beat chain: got %d, want 1", got)
	}
	if got := idx.headingLevel("Child", "9"); got != 0 {
		t.Errorf("direct sentinel 9 should mean body: got %d, want 0", got)
	}
	if got := idx.headingLevel("Named", ""); got != 4 {
		t.Errorf("name fallback: got %d, want 4", got)
	}
	if got := idx.headingLevel("Missing", ""); got != 0 {
		t.Errorf("unknown style: got %d, want 0", got)
	}
}

func TestListState_RestartSemantics(t *testing.T) {
	idx := parseNumbering([]byte(docxNumbering(
		absNum("0", [2]string{"0", "decimal"}, [2]string{"1", "decimal"}) + numRef("5", "0"))))
	ls := newListState()
	seq := []struct {
		ilvl int
		want string
	}{
		{0, "1. "}, {1, "  1. "}, {1, "  2. "}, {0, "2. "}, {1, "  1. "},
	}
	for i, s := range seq {
		got, ok := ls.marker(idx, "5", s.ilvl)
		if !ok || got != s.want {
			t.Errorf("step %d: got %q ok=%v, want %q", i, got, ok, s.want)
		}
	}
	if _, ok := ls.marker(idx, "unknown", 0); ok {
		t.Errorf("unknown numId should report ok=false")
	}
}
