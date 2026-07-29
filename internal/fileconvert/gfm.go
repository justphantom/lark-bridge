package fileconvert

import (
	"bytes"
	"encoding/csv"
	"strings"

	"github.com/justphantom/lark-bridge/internal/strutil"
)

// gfmMaxColumns is the soft limit above which a table switches from GFM pipe
// syntax to a fenced CSV block (office-extract-design.md §4.4). GFM tables
// wider than ~20 columns render as an unreadable wall of pipes that LLMs
// parse poorly and that bloat line length; CSV stays compact and its quoting
// handles embedded separators/newlines unambiguously.
const gfmMaxColumns = 20

// renderTable renders rows (rows[0] is the header) as a Markdown table, or —
// when the header spans more than gfmMaxColumns columns — as a fenced CSV
// block. Every cell in the GFM path passes through strutil.GfmCellEscape so a
// stray "|" or newline cannot break the row; the CSV path delegates quoting
// to encoding/csv. Body rows are padded/truncated to the header width so the
// table stays rectangular. Returns the rendered text ending in a newline,
// or "" when rows is empty.
func renderTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows[0]) > gfmMaxColumns {
		return renderCSVBlock(rows)
	}
	return renderGfmTable(rows)
}

// renderGfmTable emits a pipe-delimited GFM table. Cells are escaped; body
// rows are normalised to the header column count.
func renderGfmTable(rows [][]string) string {
	cols := len(rows[0])
	var b strings.Builder
	writeGfmRow := func(r []string) {
		b.WriteByte('|')
		for _, c := range r {
			b.WriteByte(' ')
			b.WriteString(strutil.GfmCellEscape(c))
			b.WriteString(" |")
		}
		b.WriteByte('\n')
	}
	writeGfmRow(rows[0])
	b.WriteByte('|')
	for range cols {
		b.WriteString(" --- |")
	}
	b.WriteByte('\n')
	for _, r := range rows[1:] {
		if len(r) < cols {
			r = append(r, make([]string, cols-len(r))...)
		} else if len(r) > cols {
			r = r[:cols]
		}
		writeGfmRow(r)
	}
	return b.String()
}

// gfmFence returns a backtick fence one longer than the longest backtick run
// in content (minimum 3): a fixed ``` fence lets content containing ``` break
// out of its own block, so the fence length adapts to the payload.
func gfmFence(content string) string {
	longest, run := 0, 0
	for i := range len(content) {
		if content[i] == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	return strings.Repeat("`", max(longest+1, 3))
}

// renderCSVBlock emits a fenced ```csv block. encoding/csv quotes cells that
// contain a comma, quote, or newline per RFC 4180, so a wide sheet survives
// intact where GFM pipes would mangle it. Writing to a bytes.Buffer never
// errors, so the WriteAll return is intentionally discarded. The fence length
// adapts to the content (gfmFence) so a cell holding ``` cannot break out.
func renderCSVBlock(rows [][]string) string {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.WriteAll(rows) //nolint:errcheck // bytes.Buffer.Write is infallible
	w.Flush()
	fence := gfmFence(buf.String())
	var b strings.Builder
	b.WriteString(fence + "csv\n")
	b.WriteString(buf.String())
	if !strings.HasSuffix(buf.String(), "\n") {
		b.WriteByte('\n')
	}
	b.WriteString(fence + "\n")
	return b.String()
}
