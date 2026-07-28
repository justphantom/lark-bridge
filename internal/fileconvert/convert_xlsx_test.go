package fileconvert

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

// buildXlsxFile builds an xlsx at a temp path by configuring a fresh
// excelize.File. Centralised so each test only states the cells that matter
// for its scenario.
func buildXlsxFile(t *testing.T, configure func(*excelize.File)) string {
	t.Helper()
	f := excelize.NewFile()
	configure(f)
	path := filepath.Join(t.TempDir(), "test.xlsx")
	if err := f.SaveAs(path); err != nil {
		t.Fatalf("save xlsx: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close xlsx: %v", err)
	}
	return path
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
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetSheetName("Sheet1", "销售明细")
		f.SetCellValue("销售明细", "A1", "订单ID")
		f.SetCellValue("销售明细", "B1", "金额")
		f.SetCellValue("销售明细", "A2", "A001")
		f.SetCellValue("销售明细", "B2", 12000)
		f.SetCellValue("销售明细", "A3", "A002")
		f.SetCellValue("销售明细", "B3", 10000)
		f.NewSheet("汇总")
		f.SetCellValue("汇总", "A1", "指标")
		f.SetCellValue("汇总", "B1", "本月")
		f.SetCellValue("汇总", "A2", "总营收")
		f.SetCellValue("汇总", "B2", "1.2M")
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
	for _, want := range []string{"# Sheet: 销售明细", "A002", "# Sheet: 汇总", "1.2M"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
}

func TestConvertXlsx_MergedCellsKeepTopLeft(t *testing.T) {
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetCellValue("Sheet1", "A1", "h1")
		f.SetCellValue("Sheet1", "B1", "h2")
		f.SetCellValue("Sheet1", "A2", "merged")
		f.MergeCell("Sheet1", "A2", "B3")
		f.SetCellValue("Sheet1", "A4", "after")
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	// decision 7A: only the top-left value survives; the rest of the range
	// renders as empty cells, keeping the table rectangular.
	if !strings.Contains(body, "| merged |  |") {
		t.Errorf("merged top-left value not isolated:\n%s", body)
	}
}

func TestConvertXlsx_FormulaValueMode(t *testing.T) {
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetCellValue("Sheet1", "A1", "h")
		f.SetCellValue("Sheet1", "B1", 5)
		f.SetCellValue("Sheet1", "B2", 5)
		f.SetCellFormula("Sheet1", "A2", "B1+B2")
	})
	body, _ := convertXlsxTo(t, New(Options{}), src) // default value mode
	if strings.Contains(body, "B1+B2") {
		t.Errorf("value mode must not leak formula text:\n%s", body)
	}
}

func TestConvertXlsx_FormulaBothMode(t *testing.T) {
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetCellValue("Sheet1", "A1", "h")
		f.SetCellValue("Sheet1", "B1", 5)
		f.SetCellValue("Sheet1", "B2", 5)
		f.SetCellFormula("Sheet1", "A2", "B1+B2")
	})
	body, _ := convertXlsxTo(t, New(Options{XlsxFormulaMode: "both"}), src)
	// no cached value → both renders "(B1+B2)"
	if !strings.Contains(body, "(B1+B2)") {
		t.Errorf("both mode should render the formula:\n%s", body)
	}
}

func TestConvertXlsx_GfmHazardSanitised(t *testing.T) {
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetCellValue("Sheet1", "A1", "h")
		f.SetCellValue("Sheet1", "A2", "a|b\nc")
	})
	body, _ := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, "a\\|b<br>c") {
		t.Errorf("pipe/newline not sanitised:\n%s", body)
	}
}

func TestConvertXlsx_EmptySheetPlaceholder(t *testing.T) {
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetCellValue("Sheet1", "A1", "h")
		f.SetCellValue("Sheet1", "A2", "v")
		f.NewSheet("Empty")
	})
	body, meta := convertXlsxTo(t, New(Options{}), src)
	if !strings.Contains(body, `<!-- Sheet "Empty": 空 sheet -->`) {
		t.Errorf("empty sheet placeholder missing:\n%s", body)
	}
	// find the Empty sheet meta
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
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetCellValue("Sheet1", "A1", "a")
		f.NewSheet("Two")
		f.SetCellValue("Two", "A1", "b")
		f.NewSheet("Three")
		f.SetCellValue("Three", "A1", "c")
	})
	_, meta := convertXlsxTo(t, New(Options{XlsxMaxSheets: 2}), src)
	if len(meta.Sheets) != 2 {
		t.Errorf("cap 2 should yield 2 sheets, got %d", len(meta.Sheets))
	}
	if meta.Sheets[1].Name != "Two" {
		t.Errorf("second sheet = %q, want Two", meta.Sheets[1].Name)
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

func TestConvertXlsx_DstMustEndMd(t *testing.T) {
	src := buildXlsxFile(t, func(f *excelize.File) {
		f.SetCellValue("Sheet1", "A1", "x")
	})
	c := New(Options{})
	if _, err := c.ConvertXlsx(context.Background(), src, src+".txt"); err == nil {
		t.Errorf("expected error when dst does not end in .md")
	}
}

// writeZipXlsx writes a raw zip with the given members. The result is NOT a
// valid spreadsheet for excelize — it exists only to exercise the
// relationship-chain scanner in detectChartsAndPivots without depending on
// excelize's chart authoring (AddChart needs a full Chart spec).
func writeZipXlsx(t *testing.T, path string, files map[string]string) {
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
	writeZipXlsx(t, path, map[string]string{
		"xl/workbook.xml":                     `<workbook ` + xlsxNS + " " + xlsxR + `><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels":          `<Relationships ` + pkgRelsNS + `>` + relXML("rId1", relWorksheet, "worksheets/sheet1.xml") + `</Relationships>`,
		"xl/worksheets/sheet1.xml":            `<worksheet ` + xlsxNS + `/>`,
		"xl/worksheets/_rels/sheet1.xml.rels": `<Relationships ` + pkgRelsNS + `>` + relXML("rId2", relDrawing, "../drawings/drawing1.xml") + `</Relationships>`,
		"xl/drawings/drawing1.xml":            `<xdr:wsDr xmlns:xdr="http://schemas.openxmlformats.org/drawingml/2006/spreadsheetDrawing"/>`,
		"xl/drawings/_rels/drawing1.xml.rels": `<Relationships ` + pkgRelsNS + `>` + relXML("rId3", relChart, "../charts/chart1.xml") + `</Relationships>`,
		"xl/charts/chart1.xml":                `<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart"/>`,
		"xl/pivotTables/pivotTable1.xml":      `<pivotTableDefinition/>`,
	})
	charts, hasPivot := detectChartsAndPivots(path)
	if charts["Sheet1"] != 1 {
		t.Errorf("chart count for Sheet1 = %d, want 1 (full chain: %v)", charts["Sheet1"], charts)
	}
	if !hasPivot {
		t.Errorf("pivot not detected")
	}
}

func TestDetectChartsAndPivots_NoChart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.xlsx")
	writeZipXlsx(t, path, map[string]string{
		"xl/workbook.xml":            `<workbook ` + xlsxNS + " " + xlsxR + `><sheets><sheet name="Sheet1" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships ` + pkgRelsNS + `>` + relXML("rId1", relWorksheet, "worksheets/sheet1.xml") + `</Relationships>`,
		"xl/worksheets/sheet1.xml":   `<worksheet ` + xlsxNS + `/>`,
	})
	charts, hasPivot := detectChartsAndPivots(path)
	if len(charts) != 0 {
		t.Errorf("expected no charts, got %v", charts)
	}
	if hasPivot {
		t.Errorf("expected no pivot")
	}
}
