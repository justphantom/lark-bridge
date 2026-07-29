package fileconvert

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// XlsxMeta is the metadata returned alongside an xlsx conversion and consumed
// by the dispatcher to build the agent prompt. Per office-extract-design.md
// §3.2 / Q11 the prompt carries ONLY path + per-sheet column names + row
// counts — never cell data. The data body is written to dstPath in full so
// the agent can Read arbitrary ranges itself.
type XlsxMeta struct {
	// Sheets mirrors the workbook's sheet order.
	Sheets []SheetMeta
	// Note carries a workbook-level caveat (e.g. pivot tables were detected
	// but could not be localised to a sheet). Empty when the workbook is
	// clean. Rendered as one extra line in the agent prompt when set.
	Note string
}

// SheetMeta describes one sheet for prompt construction.
type SheetMeta struct {
	// Name is the sheet name as stored in the workbook.
	Name string
	// Columns are the headers from the first non-empty row, with trailing
	// empty columns trimmed. nil when the sheet has no data.
	Columns []string
	// RowCount is the number of data rows (excluding the header row).
	// For a chart-only sheet this is 0.
	RowCount int
	// Note is a per-sheet caveat such as "contains 1 chart(s), not
	// extracted"; empty when the sheet is clean.
	Note string
}

// ConvertXlsx is the xlsx entry point. It writes the FULL data body to
// dstPath (decision 5C*: no truncation, no sampling) and returns sheet
// metadata so the dispatcher can build the path+schema+rows-only prompt.
// xlsx deliberately does NOT go through Convert, which has no way to return
// metadata.
func (c *Converter) ConvertXlsx(ctx context.Context, srcPath, dstPath string) (*XlsxMeta, error) {
	if !strings.HasSuffix(dstPath, ".md") {
		return nil, fmt.Errorf("fileconvert: dst must end in .md, got %q", dstPath)
	}
	return c.convertXlsx(ctx, srcPath, dstPath)
}

// convertXlsx renders an .xlsx workbook into GFM Markdown in-process
// (xlsx-extract-design.md): pure stdlib zip + streaming XML, no third-party
// dependency. Multi-sheet is always emitted in workbook order (decision
// 10A); formulas yield the cached value by default (decision 6A); merged
// cells keep only the top-left value (decision 7A, which the OOXML storage
// model honours naturally — non-anchor cells carry no <v>); charts/pivot
// tables emit HTML-comment placeholders (decision 9A) via the unchanged
// detectChartsAndPivots scanner. Per-part degradation follows §4.3: missing
// sharedStrings/styles demote features, only a broken workbook.xml is fatal.
func (c *Converter) convertXlsx(ctx context.Context, srcPath, dstPath string) (*XlsxMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	base := filepath.Base(srcPath)

	// Chart/pivot detection runs on the raw zip and is best-effort: a parse
	// failure here only means we lose a placeholder, so it must not block a
	// conversion that could otherwise complete.
	chartCounts, hasPivot := detectChartsAndPivots(srcPath)

	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return nil, fmt.Errorf("fileconvert: open xlsx %s: %w", base, err)
	}
	defer func() { _ = zr.Close() }()
	parts := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		parts[zf.Name] = zf
	}

	// Pre-pass (xlsx-extract-design.md §2.2): sheet list + date system from
	// the workbook, then the shared-string pool and the numFmt index.
	wbPart := parts["xl/workbook.xml"]
	if wbPart == nil {
		return nil, fmt.Errorf("fileconvert: xlsx %s missing xl/workbook.xml", base)
	}
	wbData, err := readZipPart(wbPart)
	if err != nil {
		return nil, fmt.Errorf("fileconvert: read workbook.xml of %s: %w", base, err)
	}
	sheets := parseWorkbookSheets(wbData)
	date1904 := parseDate1904(wbData)
	wbRels := relsMap(parts, "xl/_rels/workbook.xml.rels")
	sst := parseSharedStrings(readPartOrNil(parts, "xl/sharedStrings.xml"))
	fmts := parseNumFmts(readPartOrNil(parts, "xl/styles.xml"))

	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("fileconvert: create xlsx dst: %w", err)
	}
	// Accumulate the body in a bytes.Buffer and flush once at the end.
	// bytes.Buffer's Write never fails, so the many Write/String/Byte calls
	// below stay clean without per-call error checks; the only error that
	// matters (disk full / unwritable) surfaces on the single buf.WriteTo.
	buf := &bytes.Buffer{}

	meta := &XlsxMeta{}
	if hasPivot {
		meta.Note = "workbook contains pivot table(s), not extracted"
	}
	// Sheet-count truncation must be visible in the metadata: a prompt that
	// lists only the converted prefix would otherwise mislead the agent into
	// believing the workbook ends there.
	if c.xlsxMaxSheets > 0 && len(sheets) > c.xlsxMaxSheets {
		trunc := fmt.Sprintf("workbook has %d sheets; only first %d converted", len(sheets), c.xlsxMaxSheets)
		if meta.Note != "" {
			meta.Note += "; "
		}
		meta.Note += trunc
	}

	for i, sheet := range sheets {
		if c.xlsxMaxSheets > 0 && i >= c.xlsxMaxSheets {
			break
		}
		// Cancellation check between sheets: a huge worksheet's parse is
		// synchronous, so this is the cheap place to honour ctx.
		select {
		case <-ctx.Done():
			_ = out.Close()
			_ = os.Remove(dstPath)
			return nil, fmt.Errorf("fileconvert: xlsx %s cancelled: %w", base, ctx.Err())
		default:
		}

		// Sheet names are workbook-authored: sanitise before embedding them
		// in Markdown headings / HTML comments so a hostile name cannot forge
		// new lines or close the comment early (the output feeds an agent).
		name := sanitizeMetaText(sheet.Name)
		fmt.Fprintf(buf, "# Sheet: %s\n\n", name)
		res, perr := c.parseSheetByName(ctx, parts, wbRels, sheet, sst, fmts, date1904)
		writeXlsxSheetBody(buf, name, res, perr, chartCounts[sheet.Name])
		buf.WriteString("---\n\n")

		meta.Sheets = append(meta.Sheets, buildSheetMeta(name, res, perr, chartCounts[sheet.Name]))

		if c.log != nil {
			c.log.Debug("fileconvert: xlsx sheet converted",
				"src", base, "sheet", sheet.Name)
		}
	}

	// Global pivot caveat belongs to the whole workbook, not one sheet; emit
	// it once after the last sheet so an agent skimming the file still sees
	// that something was intentionally omitted.
	if hasPivot {
		buf.WriteString("<!-- 工作簿含数据透视表（pivotTable），未提取 -->\n\n")
	}

	if _, err := buf.WriteTo(out); err != nil {
		_ = out.Close()
		_ = os.Remove(dstPath)
		return nil, fmt.Errorf("fileconvert: write xlsx dst: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dstPath)
		return nil, fmt.Errorf("fileconvert: close xlsx dst: %w", err)
	}
	return meta, nil
}

// parseSheetByName resolves one sheet's worksheet part through the workbook
// rels and parses it. A missing/broken part is a per-sheet failure (the
// caller renders a 读取失败 placeholder and continues), never fatal.
func (c *Converter) parseSheetByName(ctx context.Context, parts map[string]*zip.File, wbRels map[string]relationship, sheet wbSheet, sst []string, fmts *numFmtIndex, date1904 bool) (*sheetResult, error) {
	rel, ok := wbRels[sheet.RID]
	if !ok || rel.External {
		return nil, fmt.Errorf("sheet %q has no worksheet relationship", sheet.Name)
	}
	wsPath := resolvePartTarget("xl", rel.Target)
	zf := parts[wsPath]
	if zf == nil {
		return nil, fmt.Errorf("worksheet part %s missing", wsPath)
	}
	data, err := readZipPart(zf)
	if err != nil {
		return nil, err
	}
	return parseSheet(ctx, data, sst, fmts, date1904, c.xlsxFormulaMode)
}

// writeXlsxSheetBody renders one sheet's body: the full data table (or a
// placeholder when the parse failed / the sheet is chart-only / empty),
// followed by the per-sheet chart placeholder and the aggregate caveats the
// parser counted (unrecognised formats, unexpanded shared formulas, uncached
// formula values, shared-string overruns — decisions Q4/Q5 and §4.3, never
// silently skipped).
func writeXlsxSheetBody(buf *bytes.Buffer, sheet string, res *sheetResult, perr error, chartCount int) {
	switch {
	case perr != nil:
		fmt.Fprintf(buf, "<!-- Sheet %q: 读取失败，未提取: %v -->\n\n", sheet, perr)
		return
	case len(res.rows) == 0 && chartCount > 0:
		fmt.Fprintf(buf, "<!-- Sheet %q: 含 %d 个图表（chart），未提取 -->\n\n", sheet, chartCount)
		return
	case len(res.rows) == 0:
		// Truly empty sheet: emit an honest marker rather than a bare heading
		// so the agent knows there was nothing to read.
		fmt.Fprintf(buf, "<!-- Sheet %q: 空 sheet -->\n\n", sheet)
		return
	default:
		buf.WriteString(renderTable(res.rows))
		buf.WriteByte('\n')
		if chartCount > 0 {
			fmt.Fprintf(buf, "<!-- Sheet %q: 含 %d 个图表（chart），未提取 -->\n\n", sheet, chartCount)
		}
	}
	if res.unknownFmt > 0 {
		fmt.Fprintf(buf, "<!-- %d 个单元格格式未识别，输出原始值 -->\n\n", res.unknownFmt)
	}
	if res.sharedFormula > 0 {
		fmt.Fprintf(buf, "<!-- %d 个 shared 公式单元格未展开，输出缓存值 -->\n\n", res.sharedFormula)
	}
	if res.noCache > 0 {
		fmt.Fprintf(buf, "<!-- 共 %d 个公式无缓存值 -->\n\n", res.noCache)
	}
	if res.sstOverrun > 0 {
		fmt.Fprintf(buf, "<!-- %d 个单元格 sharedStrings 引用越界，输出空值 -->\n\n", res.sstOverrun)
	}
}

// buildSheetMeta derives the prompt-facing metadata for one sheet from its
// parsed rows — the same rows that were just written to disk, so a large
// sheet is parsed exactly once (the excelize implementation read it twice).
func buildSheetMeta(sheet string, res *sheetResult, perr error, chartCount int) SheetMeta {
	sm := SheetMeta{Name: sheet}
	if perr != nil {
		sm.Note = "读取失败，未提取"
		return sm
	}
	sm.Columns, sm.RowCount = deriveColumnsAndRows(res.rows)
	if chartCount > 0 {
		sm.Note = fmt.Sprintf("contains %d chart(s), not extracted", chartCount)
	}
	return sm
}

// sanitizeMetaText makes a workbook-authored string (a sheet name) safe to
// embed in Markdown headings and HTML comments: ASCII control characters
// (which could forge new lines / a fake prompt) become spaces, and "--" (an
// illegal sequence inside HTML comments — "-->" would close one early) is
// replaced by an em dash.
func sanitizeMetaText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.ReplaceAll(s, "--", "—")
}

// deriveColumnsAndRows picks the first non-empty row as the header and counts
// the data rows beneath it. Trailing empty column names are trimmed so the
// prompt's "[c1, c2, …]" list does not trail with anonymous slots produced by
// ragged row lengths.
func deriveColumnsAndRows(rows [][]string) ([]string, int) {
	headerIdx := -1
	for i, r := range rows {
		if !rowIsEmpty(r) {
			headerIdx = i
			break
		}
	}
	if headerIdx < 0 {
		return nil, 0
	}
	cols := append([]string(nil), rows[headerIdx]...)
	for len(cols) > 0 && strings.TrimSpace(cols[len(cols)-1]) == "" {
		cols = cols[:len(cols)-1]
	}
	rowCount := len(rows) - headerIdx - 1
	if rowCount < 0 {
		rowCount = 0
	}
	return cols, rowCount
}

// rowIsEmpty reports whether every cell in r is blank after trimming. Used to
// skip leading blank rows when locating the header.
func rowIsEmpty(r []string) bool {
	for _, c := range r {
		if strings.TrimSpace(c) != "" {
			return false
		}
	}
	return true
}
