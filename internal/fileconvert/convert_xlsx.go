package fileconvert

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
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
// metadata without leaking the excelize dependency into the dispatcher.
func (c *Converter) ConvertXlsx(ctx context.Context, srcPath, dstPath string) (*XlsxMeta, error) {
	if !strings.HasSuffix(dstPath, ".md") {
		return nil, fmt.Errorf("fileconvert: dst must end in .md, got %q", dstPath)
	}
	return c.convertXlsx(ctx, srcPath, dstPath)
}

// convertXlsx renders an .xlsx workbook into GFM Markdown. Multi-sheet is
// always emitted in workbook order (decision 10A); formulas yield the cached
// value by default (decision 6A); merged cells keep only the top-left value
// (decision 7A, which GetRows already honours); charts/pivot tables emit
// HTML-comment placeholders (decision 9A). Charts are localised per sheet;
// pivot tables are detected globally (excelize has no read API and per-sheet
// localisation via the OOXML relationship chain is disproportionate to its
// rarity — see convert_xlsx_scan.go).
func (c *Converter) convertXlsx(ctx context.Context, srcPath, dstPath string) (*XlsxMeta, error) {
	base := filepath.Base(srcPath)

	// Chart/pivot detection runs on the raw zip and is best-effort: a parse
	// failure here only means we lose a placeholder, so it must not block a
	// conversion that excelize could otherwise complete.
	chartCounts, hasPivot := detectChartsAndPivots(srcPath)

	f, err := excelize.OpenFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("fileconvert: open xlsx %s: %w", base, err)
	}
	defer func() { _ = f.Close() }()

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

	sheets := f.GetSheetList()
	for i, sheet := range sheets {
		if c.xlsxMaxSheets > 0 && i >= c.xlsxMaxSheets {
			break
		}
		// Cancellation check between sheets: a huge workbook's GetRows is
		// synchronous, so this is the cheap place to honour ctx.
		select {
		case <-ctx.Done():
			_ = out.Close()
			return nil, fmt.Errorf("fileconvert: xlsx %s cancelled: %w", base, ctx.Err())
		default:
		}

		fmt.Fprintf(buf, "# Sheet: %s\n\n", sheet)
		c.writeXlsxSheet(buf, f, sheet, chartCounts[sheet])
		buf.WriteString("---\n\n")

		meta.Sheets = append(meta.Sheets, buildSheetMeta(f, sheet, chartCounts[sheet]))

		if c.log != nil {
			c.log.Debug("fileconvert: xlsx sheet converted",
				"src", base, "sheet", sheet)
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
		return nil, fmt.Errorf("fileconvert: write xlsx dst: %w", err)
	}
	if err := out.Close(); err != nil {
		return nil, fmt.Errorf("fileconvert: close xlsx dst: %w", err)
	}
	return meta, nil
}

// writeXlsxSheet renders one sheet's body into bw: the full data table (or a
// placeholder when GetRows fails / the sheet is chart-only), followed by any
// per-sheet chart placeholder. Row data is fetched once via GetRows, which
// joins sharedStrings, formats dates by numFmt, resolves booleans, and — per
// decision 7A — already returns only the top-left value of each merged range.
func (c *Converter) writeXlsxSheet(buf *bytes.Buffer, f *excelize.File, sheet string, chartCount int) {
	rows, err := f.GetRows(sheet)
	if err != nil {
		fmt.Fprintf(buf, "<!-- Sheet %q: 读取失败，未提取: %v -->\n\n", sheet, err)
		return
	}
	rows = c.applyFormulaMode(f, sheet, rows)

	switch {
	case len(rows) == 0 && chartCount > 0:
		fmt.Fprintf(buf, "<!-- Sheet %q: 含 %d 个图表（chart），未提取 -->\n\n", sheet, chartCount)
	case len(rows) == 0:
		// Truly empty sheet: emit an honest marker rather than a bare heading
		// so the agent knows there was nothing to read.
		fmt.Fprintf(buf, "<!-- Sheet %q: 空 sheet -->\n\n", sheet)
	default:
		buf.WriteString(renderTable(rows))
		buf.WriteByte('\n')
		if chartCount > 0 {
			fmt.Fprintf(buf, "<!-- Sheet %q: 含 %d 个图表（chart），未提取 -->\n\n", sheet, chartCount)
		}
	}
}

// applyFormulaMode rewrites cells in-place for the formula/both modes. The
// default "value" mode short-circuits and returns rows untouched — GetRows
// already yields cached results (decision 6A) and avoids the O(cells) cost of
// probing every cell for a formula, which matters on a 50k-row sheet. Only an
// operator who opts into formula/both pays that cost.
func (c *Converter) applyFormulaMode(f *excelize.File, sheet string, rows [][]string) [][]string {
	if c.xlsxFormulaMode == "value" || c.xlsxFormulaMode == "" {
		return rows
	}
	both := c.xlsxFormulaMode == "both"
	for r := range rows {
		for col := range rows[r] {
			cell, err := excelize.CoordinatesToCellName(col+1, r+1)
			if err != nil {
				continue
			}
			formula, err := f.GetCellFormula(sheet, cell)
			if err != nil || formula == "" {
				continue
			}
			val := rows[r][col]
			if both {
				if val != "" {
					rows[r][col] = val + " (" + formula + ")"
				} else {
					rows[r][col] = "(" + formula + ")"
				}
			} else {
				rows[r][col] = formula
			}
		}
	}
	return rows
}

// buildSheetMeta derives the prompt-facing metadata for one sheet from its
// rows. Columns come from the first non-empty row (with trailing empties
// trimmed); RowCount is the number of rows after that header.
func buildSheetMeta(f *excelize.File, sheet string, chartCount int) SheetMeta {
	sm := SheetMeta{Name: sheet}
	rows, err := f.GetRows(sheet)
	if err != nil {
		sm.Note = "读取失败，未提取"
		return sm
	}
	sm.Columns, sm.RowCount = deriveColumnsAndRows(rows)
	if chartCount > 0 {
		sm.Note = fmt.Sprintf("contains %d chart(s), not extracted", chartCount)
	}
	return sm
}

// deriveColumnsAndRows picks the first non-empty row as the header and counts
// the data rows beneath it. Trailing empty column names are trimmed so the
// prompt's "[c1, c2, …]" list does not trail with anonymous slots produced by
// ragged row lengths from excelize.
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
