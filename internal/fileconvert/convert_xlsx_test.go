package fileconvert

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// xlsx fixture builders. Fixtures are authored inline as OOXML inside a zip —
// reproducible and dependency-free, no excelize and no MS Excel required in
// CI (xlsx-extract-design.md §5.1).

const sDecl = `xmlns="` + nsS + `" xmlns:r="` + ooRelsNS + `"`

// xlsxSheet pairs a sheet name with its worksheet XML body (the <sheetData>
// rows, not a full worksheet document).
type xlsxSheet struct {
	name string
	rows string
}

// buildXlsx assembles a fixture workbook on disk from its sheets plus any
// extra parts (sharedStrings.xml / styles.xml). Sheets are numbered
// sheet1.xml… in slice order with matching rIds.
func buildXlsx(t *testing.T, sheets []xlsxSheet, extra map[string]string) string {
	t.Helper()
	var wbSheets, rels strings.Builder
	files := map[string]string{}
	for i, sh := range sheets {
		rid := "rId" + itoa(i+1)
		part := "worksheets/sheet" + itoa(i+1) + ".xml"
		wbSheets.WriteString(`<sheet name="` + sh.name + `" sheetId="` + itoa(i+1) + `" r:id="` + rid + `"/>`)
		rels.WriteString(relXML(rid, relWorksheet, part))
		files["xl/"+part] = `<worksheet ` + sDecl + `><sheetData>` + sh.rows + `</sheetData></worksheet>`
	}
	files["xl/workbook.xml"] = `<workbook ` + sDecl + `><sheets>` + wbSheets.String() + `</sheets></workbook>`
	files["xl/_rels/workbook.xml.rels"] = `<Relationships ` + pkgRelsNS + `>` + rels.String() + `</Relationships>`
	for k, v := range extra {
		files[k] = v
	}
	src := filepath.Join(t.TempDir(), "test.xlsx")
	writeZip(t, src, files)
	return src
}

// xR wraps cells in a <row>.
func xR(cells ...string) string {
	return `<row>` + strings.Join(cells, "") + `</row>`
}

// xC builds a cell with an optional type attribute and inner XML (<v>/<f>).
func xC(ref, typ, inner string) string {
	t := ""
	if typ != "" {
		t = ` t="` + typ + `"`
	}
	return `<c r="` + ref + `"` + t + `>` + inner + `</c>`
}

// xCs builds a numeric cell carrying a style index.
func xCs(ref string, sty int, val string) string {
	return `<c r="` + ref + `" s="` + itoa(sty) + `"><v>` + val + `</v></c>`
}

// xV wraps a value; xF wraps a formula; xFshared emits a shared-formula
// master or follower.
func xV(v string) string { return `<v>` + v + `</v>` }
func xF(f string) string { return `<f>` + f + `</f>` }
func xFsharedMaster(f string) string {
	return `<f t="shared" ref="A2:A3" si="0">` + f + `</f>`
}
func xFsharedFollower() string { return `<f t="shared" si="0"/>` }

// sstXML builds xl/sharedStrings.xml from plain-text entries.
func sstXML(texts ...string) string {
	var b strings.Builder
	b.WriteString(`<sst ` + sDecl + `>`)
	for _, s := range texts {
		b.WriteString(`<si><t>` + s + `</t></si>`)
	}
	b.WriteString(`</sst>`)
	return b.String()
}

// stylesXML builds xl/styles.xml from raw numFmts + cellXfs fragments.
func stylesXML(numFmts, cellXfs string) string {
	s := `<styleSheet ` + sDecl + `>`
	if numFmts != "" {
		s += `<numFmts count="1">` + numFmts + `</numFmts>`
	}
	return s + `<cellXfs count="2">` + cellXfs + `</cellXfs></styleSheet>`
}

func convertXlsxTo(t *testing.T, c *Converter, src string) (string, *XlsxMeta) {
	t.Helper()
	dst := strings.TrimSuffix(src, ".xlsx") + ".md"
	meta, err := c.ConvertXlsx(context.Background(), src, dst)
	if err != nil {
		t.Fatalf("ConvertXlsx: %v", err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	return string(body), meta
}

func TestConvertXlsx_BasicMultiSheet(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"销售明细", xR(
			xC("A1", "s", xV("0"))+xC("B1", "s", xV("1")),
		) + xR(
			xC("A2", "s", xV("2"))+xC("B2", "", xV("12000")),
		) + xR(
			xC("A3", "s", xV("3"))+xC("B3", "", xV("10000")),
		)},
		{"汇总", xR(
			xC("A1", "s", xV("4"))+xC("B1", "s", xV("5")),
		) + xR(
			xC("A2", "s", xV("6"))+xC("B2", "s", xV("7")),
		)},
	}, map[string]string{
		"xl/sharedStrings.xml": sstXML("订单ID", "金额", "A001", "A002", "指标", "本月", "总营收", "1.2M"),
	})
	body, meta := convertXlsxTo(t, New(Options{}), src)

	if len(meta.Sheets) != 2 {
		t.Fatalf("want 2 sheets, got %d", len(meta.Sheets))
	}
	if meta.Sheets[0].Name != "销售明细" {
		t.Errorf("sheet0 name = %q, want 销售明细", meta.Sheets[0].Name)
	}
	if got := meta.Sheets[0].RowCount; got != 2 {
		t.Errorf("sheet0 rows = %d, want 2", got)
	}
	if got := strings.Join(meta.Sheets[0].Columns, ","); got != "订单ID,金额" {
		t.Errorf("sheet0 cols = %q, want 订单ID,金额", got)
	}
	if meta.Sheets[1].Name != "汇总" || meta.Sheets[1].RowCount != 1 {
		t.Errorf("sheet1 meta = %+v", meta.Sheets[1])
	}
	// C-paradigm assertions (§6.2): full body on disk, every row present.
	for _, want := range []string{"# Sheet: 销售明细", "A002", "12000", "# Sheet: 汇总", "1.2M"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestConvertXlsx_MergedCellsKeepTopLeft(t *testing.T) {
	// decision 7A: non-anchor cells of a merged range carry no <v> in OOXML,
	// so only the top-left value survives without any merge-range parsing.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("0"))+xC("B1", "", xV("0"))) +
			xR(xC("A2", "s", xV("0"))) +
			xR(xC("A4", "s", xV("1")))},
	}, map[string]string{
		"xl/sharedStrings.xml": sstXML("merged", "after"),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "| merged |  |") {
		t.Errorf("merged top-left value not isolated:\n%s", body)
	}
}

func TestConvertXlsx_FormulaValueMode(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "s", xV("0"))+xC("B1", "", xV("5"))) +
			xR(xC("A2", "", xF("B1+B2")+xV("10")))},
	}, map[string]string{
		"xl/sharedStrings.xml": sstXML("h"),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src) // default value mode
	if !strings.Contains(body, "10") {
		t.Errorf("value mode should render the cached value:\n%s", body)
	}
	if strings.Contains(body, "B1+B2") {
		t.Errorf("value mode must not leak formula text:\n%s", body)
	}
}

func TestConvertXlsx_FormulaFormulaMode(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("5"))) +
			xR(xC("A2", "", xF("B1+B2")+xV("10")))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{XlsxFormulaMode: "formula"}), src)
	if !strings.Contains(body, "B1+B2") {
		t.Errorf("formula mode should render the formula:\n%s", body)
	}
}

func TestConvertXlsx_FormulaBothMode(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("5"))) +
			xR(xC("A2", "", xF("B1+B2")))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{XlsxFormulaMode: "both"}), src)
	// no cached value → both renders "(B1+B2)"
	if !strings.Contains(body, "(B1+B2)") {
		t.Errorf("both mode should render the formula:\n%s", body)
	}
}

func TestConvertXlsx_FormulaBothModeWithCache(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("5"))) +
			xR(xC("A2", "", xF("B1+B2")+xV("10")))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{XlsxFormulaMode: "both"}), src)
	if !strings.Contains(body, "10 (B1+B2)") {
		t.Errorf("both mode should render value + formula:\n%s", body)
	}
}

func TestConvertXlsx_FormulaSharedFollower(t *testing.T) {
	// decision Q5 caveat: shared-formula followers keep their cached value
	// and are counted into an aggregate placeholder.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("5"))) +
			xR(xC("A2", "", xFsharedMaster("B1*2")+xV("10"))) +
			xR(xC("A3", "", xFsharedFollower()+xV("20")))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{XlsxFormulaMode: "formula"}), src)
	if !strings.Contains(body, "B1*2") || !strings.Contains(body, "20") {
		t.Errorf("shared master formula / follower value missing:\n%s", body)
	}
	if !strings.Contains(body, "<!-- 1 个 shared 公式单元格未展开，输出缓存值 -->") {
		t.Errorf("shared-formula placeholder missing:\n%s", body)
	}
}

func TestConvertXlsx_FormulaNoCache(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("5"))) +
			xR(xC("A2", "", xF("B1+B2")))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{}), src) // value mode
	if !strings.Contains(body, "<!-- 共 1 个公式无缓存值 -->") {
		t.Errorf("no-cache placeholder missing:\n%s", body)
	}
}

func TestConvertXlsx_SharedStringRichText(t *testing.T) {
	// Rich-text <si> concatenates all <r>/<t> runs.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "s", xV("0")))},
	}, map[string]string{
		"xl/sharedStrings.xml": `<sst ` + sDecl + `><si>` +
			`<r><rPr><b/></rPr><t>富文</t></r><r><t>本拼接</t></r>` +
			`</si></sst>`,
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "富文本拼接") {
		t.Errorf("rich-text runs not concatenated:\n%s", body)
	}
}

func TestConvertXlsx_SharedStringPhoneticSkipped(t *testing.T) {
	// <rPh> carries furigana readings — annotation, not body text.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "s", xV("0")))},
	}, map[string]string{
		"xl/sharedStrings.xml": `<sst ` + sDecl + `><si>` +
			`<rPh sb="0" eb="1"><t>よみ</t></rPh><r><t>読</t></r>` +
			`</si></sst>`,
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "読") || strings.Contains(body, "よみ") {
		t.Errorf("phonetic text should be skipped:\n%s", body)
	}
}

func TestConvertXlsx_InlineString(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "inlineStr", `<is><t>内联</t></is>`))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "内联") {
		t.Errorf("inlineStr not extracted:\n%s", body)
	}
}

func TestConvertXlsx_BoolAndError(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(
			xC("A1", "b", xV("1")) + xC("B1", "b", xV("0")) + xC("C1", "e", xV("#REF!")))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{}), src)
	for _, want := range []string{"TRUE", "FALSE", "#REF!"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
}

func TestConvertXlsx_DateBuiltinFormats(t *testing.T) {
	// serial 44927 = 2023-01-01; 44927.5 = noon. Styles: xf0=General,
	// xf1=numFmtId 14 (date), xf2=22 (datetime), xf3=18 (time), xf4=31 (CJK).
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(
			xCs("A1", 1, "44927") + xCs("B1", 2, "44927.5") + xCs("C1", 3, "0.5") + xCs("D1", 4, "44927"))},
	}, map[string]string{
		"xl/styles.xml": stylesXML("", `<xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="22"/><xf numFmtId="18"/><xf numFmtId="31"/>`),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	for _, want := range []string{"2023-01-01", "2023-01-01 12:00:00", "12:00:00"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "44927") {
		t.Errorf("date serial leaked raw:\n%s", body)
	}
}

func TestConvertXlsx_DateCustomPattern(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xCs("A1", 1, "44927") + xCs("B1", 2, "44927.5"))},
	}, map[string]string{
		"xl/styles.xml": stylesXML(
			`<numFmt numFmtId="164" formatCode="yyyy-mm-dd"/><numFmt numFmtId="165" formatCode="dd/mm/yyyy h:mm"/>`,
			`<xf numFmtId="0"/><xf numFmtId="164"/><xf numFmtId="165"/>`),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "2023-01-01") || !strings.Contains(body, "2023-01-01 12:00:00") {
		t.Errorf("custom date patterns not recognised:\n%s", body)
	}
}

func TestConvertXlsx_DateUnknownPatternFallback(t *testing.T) {
	// decision Q4: an unrecognised custom pattern keeps the raw serial and
	// is counted into an aggregate placeholder.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xCs("A1", 1, "44927"))},
	}, map[string]string{
		"xl/styles.xml": stylesXML(
			`<numFmt numFmtId="164" formatCode="m0.00"/>`,
			`<xf numFmtId="0"/><xf numFmtId="164"/>`),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "44927") {
		t.Errorf("unknown pattern should pass the raw value through:\n%s", body)
	}
	if !strings.Contains(body, "<!-- 1 个单元格格式未识别，输出原始值 -->") {
		t.Errorf("unknown-format placeholder missing:\n%s", body)
	}
}

func TestConvertXlsx_Date1904Epoch(t *testing.T) {
	// 1904 date system: serial 1 → 1904-01-02.
	src := buildXlsxDate1904(t, xR(xCs("A1", 1, "1")))
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "1904-01-02") {
		t.Errorf("1904 epoch not applied:\n%s", body)
	}
}

// buildXlsxDate1904 is buildXlsx with <workbookPr date1904="1"/> injected.
func buildXlsxDate1904(t *testing.T, rows string) string {
	t.Helper()
	files := map[string]string{
		"xl/workbook.xml": `<workbook ` + sDecl + `><workbookPr date1904="1"/><sheets>` +
			`<sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships ` + pkgRelsNS + `>` +
			relXML("rId1", relWorksheet, "worksheets/sheet1.xml") + `</Relationships>`,
		"xl/worksheets/sheet1.xml": `<worksheet ` + sDecl + `><sheetData>` + rows + `</sheetData></worksheet>`,
		"xl/styles.xml":            stylesXML("", `<xf numFmtId="0"/><xf numFmtId="14"/>`),
	}
	src := filepath.Join(t.TempDir(), "test.xlsx")
	writeZip(t, src, files)
	return src
}

func TestConvertXlsx_NumberRawPassthrough(t *testing.T) {
	// decision Q2: display formats (percent/thousands) are not applied; the
	// raw <v> is the canonical value.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xCs("A1", 1, "0.15") + xCs("B1", 2, "1200"))},
	}, map[string]string{
		"xl/styles.xml": stylesXML(
			`<numFmt numFmtId="164" formatCode="0.00%"/><numFmt numFmtId="165" formatCode="#,##0"/>`,
			`<xf numFmtId="0"/><xf numFmtId="164"/><xf numFmtId="165"/>`),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "0.15") || !strings.Contains(body, "1200") {
		t.Errorf("raw values not passed through:\n%s", body)
	}
	if strings.Contains(body, "15%") || strings.Contains(body, "1,200") {
		t.Errorf("display format should not be applied:\n%s", body)
	}
}

func TestConvertXlsx_SparseRowsPadded(t *testing.T) {
	// Skipped columns (no <c> at all) pad to empty strings.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "s", xV("0"))+xC("C1", "s", xV("1"))) +
			xR(xC("A2", "s", xV("2"))+xC("B2", "s", xV("3"))+xC("C2", "s", xV("4")))},
	}, map[string]string{
		"xl/sharedStrings.xml": sstXML("h1", "h3", "a", "b", "c"),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "| h1 |  | h3 |") {
		t.Errorf("sparse gap not padded:\n%s", body)
	}
}

func TestConvertXlsx_SstMissingDegrades(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "s", xV("0")) + xC("B1", "", xV("42")))},
	}, nil) // no sharedStrings.xml at all
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "42") {
		t.Errorf("numeric cells should survive sst loss:\n%s", body)
	}
	if !strings.Contains(body, "sharedStrings 引用越界") {
		t.Errorf("sst-overrun placeholder missing:\n%s", body)
	}
}

func TestConvertXlsx_StylesMissingDegrades(t *testing.T) {
	// No styles.xml: a styled date cell falls back to the raw serial.
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xCs("A1", 3, "44927"))},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "44927") {
		t.Errorf("styles loss should pass raw values through:\n%s", body)
	}
}

func TestConvertXlsx_GfmHazardSanitised(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "s", xV("0"))) + xR(xC("A2", "s", xV("1")))},
	}, map[string]string{
		"xl/sharedStrings.xml": sstXML("h", "a|b\nc"),
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "a\\|b<br>c") {
		t.Errorf("pipe/newline not sanitised:\n%s", body)
	}
}

func TestConvertXlsx_WideTableDegradesToCSV(t *testing.T) {
	var cells []string
	for range 21 {
		cells = append(cells, xC("", "", xV("0")))
	}
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(cells...)},
	}, nil)
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "```csv") {
		t.Errorf("wide table should degrade to fenced CSV:\n%s", body)
	}
}

func TestConvertXlsx_EmptySheetPlaceholder(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "s", xV("0"))) + xR(xC("A2", "s", xV("1")))},
		{"Empty", ""},
	}, map[string]string{
		"xl/sharedStrings.xml": sstXML("h", "v"),
	})
	body, meta := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, `<!-- Sheet "Empty": 空 sheet -->`) {
		t.Errorf("empty sheet placeholder missing:\n%s", body)
	}
	var empty *SheetMeta
	for i := range meta.Sheets {
		if meta.Sheets[i].Name == "Empty" {
			empty = &meta.Sheets[i]
		}
	}
	if empty == nil {
		t.Fatalf("Empty sheet not in meta")
	}
	if empty.RowCount != 0 || len(empty.Columns) != 0 {
		t.Errorf("Empty sheet should have 0 rows/cols, got %+v", empty)
	}
}

func TestConvertXlsx_MaxSheetsCap(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("1")))},
		{"Two", xR(xC("A1", "", xV("2")))},
		{"Three", xR(xC("A1", "", xV("3")))},
	}, nil)
	_, meta := convertXlsxTo(t, New(Options{XlsxMaxSheets: 2}), src)
	if len(meta.Sheets) != 2 {
		t.Errorf("cap 2 should yield 2 sheets, got %d", len(meta.Sheets))
	}
	if meta.Sheets[1].Name != "Two" {
		t.Errorf("second sheet = %q, want Two", meta.Sheets[1].Name)
	}
}

func TestConvertXlsx_CancelledCtxAborts(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("1")))},
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst := strings.TrimSuffix(src, ".xlsx") + ".md"
	if _, err := New(Options{}).ConvertXlsx(ctx, src, dst); err == nil {
		t.Errorf("expected error on cancelled ctx, got nil")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("dst should not exist after cancelled conversion")
	}
}

func TestConvertXlsx_CorruptFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "bad.xlsx")
	if err := os.WriteFile(src, []byte("not a zip"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := New(Options{})
	if _, err := c.ConvertXlsx(context.Background(), src, src+".md"); err == nil {
		t.Errorf("expected error on corrupt xlsx, got nil")
	}
}

func TestConvertXlsx_MissingWorkbookFails(t *testing.T) {
	src := filepath.Join(t.TempDir(), "nowb.xlsx")
	writeZip(t, src, map[string]string{
		"xl/worksheets/sheet1.xml": `<worksheet ` + sDecl + `><sheetData/></worksheet>`,
	})
	if _, err := New(Options{}).ConvertXlsx(context.Background(), src, src+".md"); err == nil {
		t.Errorf("expected error when xl/workbook.xml missing, got nil")
	}
}

func TestConvertXlsx_DstMustEndMd(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"Sheet1", xR(xC("A1", "", xV("1")))},
	}, nil)
	c := New(Options{})
	if _, err := c.ConvertXlsx(context.Background(), src, src+".txt"); err == nil {
		t.Errorf("expected error when dst does not end in .md")
	}
}

func TestClassifyFormat(t *testing.T) {
	cases := []struct {
		code string
		want valClass
	}{
		{"yyyy-mm-dd", classDate},
		{`yyyy"年"m"月"d"日"`, classDate},
		{"dd/mm/yyyy h:mm", classDatetime},
		{"h:mm:ss", classTime},
		{"mm:ss", classTime},
		{"0.00%", classNumber},
		{"#,##0", classNumber},
		{"0.00E+00", classNumber},
		{"General", classNumber},
		{"@", classNumber},
		{`[Red]0.00`, classNumber},
		{`0.00_);[Red]\(0.00\)`, classNumber},
		{"m/d/yy;@", classDate}, // condition sections: first wins
		{"m0.00", classUnknownDate},
	}
	for _, tc := range cases {
		if got := classifyFormat(tc.code); got != tc.want {
			t.Errorf("classifyFormat(%q) = %v, want %v", tc.code, got, tc.want)
		}
	}
}

func TestBuiltinDateClass(t *testing.T) {
	for id, want := range map[int]valClass{
		14: classDate, 17: classDate, 18: classTime, 21: classTime,
		22: classDatetime, 27: classDate, 36: classDate, 45: classTime,
		47: classTime, 50: classDate, 58: classDate,
	} {
		if got, ok := builtinDateClass(id); !ok || got != want {
			t.Errorf("builtinDateClass(%d) = %v,%v want %v,true", id, got, ok, want)
		}
	}
	for _, id := range []int{0, 1, 9, 10, 13, 23, 44, 48, 49, 59, 164} {
		if _, ok := builtinDateClass(id); ok {
			t.Errorf("builtinDateClass(%d) should not be a date", id)
		}
	}
}

func TestSerialToISO(t *testing.T) {
	cases := []struct {
		serial   float64
		cls      valClass
		date1904 bool
		want     string
	}{
		{44927, classDate, false, "2023-01-01"},
		{44927.5, classDatetime, false, "2023-01-01 12:00:00"},
		{0.5, classTime, false, "12:00:00"},
		{61, classDate, false, "1900-03-01"}, // first day past the 1900 leap bug
		{1, classDate, true, "1904-01-02"},
	}
	for _, tc := range cases {
		if got := serialToISO(tc.serial, tc.cls, tc.date1904); got != tc.want {
			t.Errorf("serialToISO(%v,%v,%v) = %q, want %q",
				tc.serial, tc.cls, tc.date1904, got, tc.want)
		}
	}
}

func TestColLettersToIdx(t *testing.T) {
	cases := map[string]int{"A1": 0, "B12": 1, "Z9": 25, "AA1": 26, "BC7": 54, "": 0}
	for ref, want := range cases {
		if got := colLettersToIdx(ref); got != want {
			t.Errorf("colLettersToIdx(%q) = %d, want %d", ref, got, want)
		}
	}
}

// writeZip writes a raw zip with the given members. Used by the docx / pptx /
// xlsx fixture builders to author OOXML parts inline — reproducible and
// dependency-free.
func writeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

const (
	xlsxNS       = `xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"`
	xlsxR        = `xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"`
	pkgRelsNS    = `xmlns="http://schemas.openxmlformats.org/package/2006/relationships"`
	relDrawing   = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/drawing"
	relChart     = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart"
	relWorksheet = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"
)

func relXML(id, typ, target string) string {
	return `<Relationship Id="` + id + `" Type="` + typ + `" Target="` + target + `"/>`
}

func TestDetectChartsAndPivots_ChartChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "charts.xlsx")
	writeZip(t, path, map[string]string{
		"xl/workbook.xml":                     `<workbook ` + xlsxNS + " " + xlsxR + `><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels":          `<Relationships ` + pkgRelsNS + `>` + relXML("rId1", relWorksheet, "worksheets/sheet1.xml") + `</Relationships>`,
		"xl/worksheets/_rels/sheet1.xml.rels": `<Relationships ` + pkgRelsNS + `>` + relXML("rId2", relDrawing, "../drawings/drawing1.xml") + `</Relationships>`,
		"xl/drawings/_rels/drawing1.xml.rels": `<Relationships ` + pkgRelsNS + `>` + relXML("rId3", relChart, "../charts/chart1.xml") + `</Relationships>`,
	})
	counts, hasPivot := detectChartsAndPivots(path)
	if counts["Sheet1"] != 1 {
		t.Errorf("chart count = %v, want Sheet1:1", counts)
	}
	if hasPivot {
		t.Errorf("hasPivot = true, want false")
	}
}

func TestDetectChartsAndPivots_NoChart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.xlsx")
	writeZip(t, path, map[string]string{
		"xl/workbook.xml":            `<workbook ` + xlsxNS + " " + xlsxR + `><sheets><sheet name="S" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships ` + pkgRelsNS + `>` + relXML("rId1", relWorksheet, "worksheets/sheet1.xml") + `</Relationships>`,
	})
	counts, hasPivot := detectChartsAndPivots(path)
	if len(counts) != 0 || hasPivot {
		t.Errorf("got counts=%v pivot=%v, want empty/false", counts, hasPivot)
	}
}

func TestParseSheet_HostileSstIndex(t *testing.T) {
	// An overflowing shared-string index must count as an overrun, never a
	// panic or a wrong sst[0] lookup.
	res, err := parseSheet(context.Background(),
		[]byte(`<worksheet `+sDecl+`><sheetData>`+xR(xC("A1", "s", xV("9999999999999999999"))+xC("B1", "s", xV("abc")))+`</sheetData></worksheet>`),
		[]string{"zero"}, nil, false, "value")
	if err != nil {
		t.Fatalf("parseSheet: %v", err)
	}
	if res.sstOverrun != 2 {
		t.Errorf("sstOverrun = %d, want 2 (overflow + garbage)", res.sstOverrun)
	}
	// Both cells emitted empty values, so the row is dropped as fully empty.
	if len(res.rows) != 0 {
		t.Errorf("hostile cells should emit empty values, got %v", res.rows)
	}
}

func TestColLettersToIdx_HostileClamp(t *testing.T) {
	if got := colLettersToIdx("ZZZZZZZ1"); got != xlsxMaxColumns-1 {
		t.Errorf("colLettersToIdx(ZZZZZZZ1) = %d, want clamp %d", got, xlsxMaxColumns-1)
	}
	if got := colLettersToIdx("XFD1"); got != xlsxMaxColumns-1 {
		t.Errorf("colLettersToIdx(XFD1) = %d, want %d", got, xlsxMaxColumns-1)
	}
}

func TestAtoiSafe_Overflow(t *testing.T) {
	if got := atoiSafe("99999999999999999999999"); got != 0 {
		t.Errorf("atoiSafe overflow = %d, want 0 (garbage)", got)
	}
	if got := atoiSafe("42"); got != 42 {
		t.Errorf("atoiSafe(42) = %d", got)
	}
}

func TestParseSheet_EmptyBoolCell(t *testing.T) {
	res, err := parseSheet(context.Background(),
		[]byte(`<worksheet `+sDecl+`><sheetData>`+xR(xC("A1", "b", "")+xC("B1", "b", xV("1")))+`</sheetData></worksheet>`),
		nil, nil, false, "value")
	if err != nil {
		t.Fatalf("parseSheet: %v", err)
	}
	if len(res.rows) != 1 || res.rows[0][0] != "" || res.rows[0][1] != "TRUE" {
		t.Errorf("empty bool should stay empty, got %v", res.rows)
	}
}

func TestConvertXlsx_SheetNameSanitised(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"evil--> \nname", xR(xC("A1", "", xV("1")))},
	}, nil)
	body, meta := convertXlsxTo(t, New(Options{}), src)
	if strings.Contains(body, "--> \n") || strings.Contains(body, "evil-->") {
		t.Errorf("sheet name broke the HTML comment / forged a line:\n%s", body)
	}
	if len(meta.Sheets) != 1 || strings.ContainsAny(meta.Sheets[0].Name, "\n") {
		t.Errorf("meta sheet name not sanitised: %+v", meta.Sheets[0].Name)
	}
}

func TestConvertXlsx_MaxSheetsNote(t *testing.T) {
	src := buildXlsx(t, []xlsxSheet{
		{"S1", xR(xC("A1", "", xV("1")))},
		{"S2", xR(xC("A1", "", xV("2")))},
	}, nil)
	_, meta := convertXlsxTo(t, New(Options{XlsxMaxSheets: 1}), src)
	if !strings.Contains(meta.Note, "2 sheets") || !strings.Contains(meta.Note, "first 1") {
		t.Errorf("truncation note missing: %q", meta.Note)
	}
}

// TestWriteXlsxSheetBody_ErrorSanitised locks in R1: a parse error carrying
// attacker-controlled text (a zip part name / rels target with "-->") must be
// sanitised before embedding in the HTML comment, so it cannot close the
// comment early and inject markdown into the file the agent later Reads.
func TestWriteXlsxSheetBody_ErrorSanitised(t *testing.T) {
	var buf bytes.Buffer
	hostile := fmt.Errorf("worksheet part xl/ev-->il/sheet1.xml missing")
	writeXlsxSheetBody(&buf, "ok", nil, hostile, 0)
	got := buf.String()
	if strings.Contains(got, "ev-->il") {
		t.Errorf("error text not sanitised; \"-->\" can close the HTML comment early:\n%s", got)
	}
	if !strings.Contains(got, "读取失败") || !strings.HasSuffix(strings.TrimSpace(got), "-->") {
		t.Errorf("placeholder shape changed:\n%s", got)
	}
}

// TestZipPartLimit locks in R2: metadata parts (*.rels, workbook/presentation
// indexes) get the tight cap; data-bearing parts keep the generous ceiling.
func TestZipPartLimit(t *testing.T) {
	cases := []struct {
		name string
		want int64
	}{
		{"xl/_rels/workbook.xml.rels", maxZipMetaPartSize},
		{"xl/workbook.xml", maxZipMetaPartSize},
		{"ppt/presentation.xml", maxZipMetaPartSize},
		{"ppt/_rels/presentation.xml.rels", maxZipMetaPartSize},
		{"word/_rels/document.xml.rels", maxZipMetaPartSize},
		{"xl/worksheets/sheet1.xml", maxZipPartSize},
		{"xl/sharedStrings.xml", maxZipPartSize},
		{"word/document.xml", maxZipPartSize},
		{"xl/styles.xml", maxZipPartSize},
	}
	for _, tc := range cases {
		if got := zipPartLimit(tc.name); got != tc.want {
			t.Errorf("zipPartLimit(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}
