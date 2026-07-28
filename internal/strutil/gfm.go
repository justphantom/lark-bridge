package strutil

import "strings"

// GfmCellEscape sanitises one cell/text value for safe injection into a
// GitHub-flavoured Markdown table cell. GFM tables are pipe-delimited, so a
// raw "|" would be read as a column separator and silently corrupt the table
// layout; a raw newline would break the row. The escapes below keep each
// value on a single logical line while preserving the visible content the
// agent needs to reason about (office-extract-design.md §4.4):
//
//   - "\r" is dropped (CRLF / lone CR collapse to a single line break).
//   - "\n" becomes "<br>" so multi-line cell text stays inside one row.
//   - "|" becomes "\|" so it does not split the column.
//   - A cell that is entirely whitespace collapses to "" so empty cells keep
//     column alignment without leaking stray spaces.
//
// Note the pipe is escaped AFTER the newline substitution so the literal
// backslash introduced by escaping cannot be mangled by an earlier step.
func GfmCellEscape(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "<br>")
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}
