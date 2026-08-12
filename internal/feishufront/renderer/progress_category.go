package renderer

import (
	"strconv"
	"strings"
)

// toolCategory labels a tool row for the grouped summary, classifying by
// normalised name into read/exec/edit/write/mcp. Unclassified tools
// (WebSearch/Cron/Worktree/MultiEdit/…) return "" and are omitted from the
// summary rather than reserved — they appear in the recent-rows window.
//
// miniagent v3.2.0 removed the standalone `multi_edit` tool (merged into
// `edit`'s `edits` array, edd6ba5/S3), so a `Multi_edit` row from miniagent is
// no longer emitted; the case for it was dropped. claude's `MultiEdit`
// (distinct PascalCase name) was never classified and stays in the unclassified
// bucket — this is not a regression, just an explicit pin.
func toolCategory(t toolRow) string {
	switch {
	case isReadTool(t.name):
		return "read"
	// Bash is claude/opencode/omp's shell tool; Shell (normalised from
	// miniagent's lowercase "shell") is the same category.
	case t.name == "Bash" || t.name == "Shell":
		return "exec"
	case t.name == "Edit":
		return "edit"
	case t.name == "Write":
		return "write"
	case strings.HasPrefix(t.name, "mcp:"):
		return "mcp"
	}
	return ""
}

// categoryLabel renders one category's count for the summary line. Returns ""
// when count is 0 so the summary omits empty segments.
func categoryLabel(cat string, count int) string {
	if count <= 0 {
		return ""
	}
	var label string
	switch cat {
	case "read":
		label = "读取"
	case "exec":
		label = "执行"
	case "edit":
		label = "编辑"
	case "write":
		label = "写入"
	case "mcp":
		label = "mcp"
	default:
		return ""
	}
	return label + " " + strconv.Itoa(count)
}

// categoryTotals sums each tool row's folded count by category. Shared between
// the progress card's grouped summary and the result card's Summary() so the
// two stay in sync. Counts each row's count (folded same name+desc calls), so
// 127 reads of distinct files still total 127 even though they span 127
// distinct rows.
func categoryTotals(tools []toolRow) map[string]int {
	totals := map[string]int{}
	for _, t := range tools {
		if cat := toolCategory(t); cat != "" {
			totals[cat] += t.count
		}
	}
	return totals
}

// groupedSummary renders the "… 另完成 读取 N · 执行 N …" line for completed
// tools beyond maxCompletedTools. order fixes the segment order regardless of
// map iteration. Returns "" when no category has a count.
func groupedSummary(totals map[string]int) string {
	var parts []string
	for _, cat := range []string{"read", "exec", "edit", "write", "mcp"} {
		if lbl := categoryLabel(cat, totals[cat]); lbl != "" {
			parts = append(parts, lbl)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "… 另完成 " + strings.Join(parts, " · ")
}

// Summary builds a one-line execution digest for the result card from the
// accumulated tool rows, e.g. "📎 读取 77 · 执行 12 · 编辑 15 · mcp 32".
// Returns "" when no tools ran. Shares categoryTotals with the progress card's
// grouped summary so the in-flight digest and the final digest agree, and the
// category set covers the high-frequency tools observed in real streams
// (Read/Bash/Edit/Write/MCP); low-frequency tools (WebSearch/Cron/…) are
// omitted rather than reserved.
func (s *ProgressState) Summary() string {
	totals := categoryTotals(s.tools)
	var parts []string
	for _, cat := range []string{"read", "exec", "edit", "write", "mcp"} {
		if lbl := categoryLabel(cat, totals[cat]); lbl != "" {
			parts = append(parts, lbl)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "📎 " + strings.Join(parts, " · ")
}

// isReadTool reports whether a tool name is a read/lookup tool whose output is
// inspection-only (no side effects): Read, Grep, Glob.
func isReadTool(name string) bool {
	switch name {
	case "Read", "Grep", "Glob":
		return true
	}
	return false
}
