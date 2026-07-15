package renderer

import (
	"strconv"
	"strings"
)

// toolCategory labels a tool row for the grouped summary. The order matters:
// subagent is checked first (claude renders subagents as "<Type> Agent" /
// "Shell"; a subagent whose name coincidentally starts with "Read" must not be
// miscounted as a read), then read/exec/edit/write/mcp by normalised name.
// Unclassified tools (WebSearch/Cron/Worktree/…) return "" and are omitted from
// the summary rather than reserved — they appear in the recent-rows window.
func toolCategory(t toolRow) string {
	if t.isSubagent {
		return "sub"
	}
	switch {
	case isReadTool(t.name):
		return "read"
	case t.name == "Bash":
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
	case "sub":
		label = "子代理"
	default:
		return ""
	}
	return label + " " + strconv.Itoa(count)
}

// categoryTotals sums each tool row's folded count by category. Shared between
// the progress card's grouped summary and the result card's Summary() so the
// two stay in sync. Counts each row's count (folded same name+desc / taskID
// calls), so 127 reads of distinct files still total 127 even though they span
// 127 distinct rows.
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
	for _, cat := range []string{"read", "exec", "edit", "write", "mcp", "sub"} {
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
// accumulated tool rows, e.g. "📎 读取 77 · 执行 12 · 编辑 15 · mcp 32 · 子代理 1".
// Returns "" when no tools ran. Shares categoryTotals with the progress card's
// grouped summary so the in-flight digest and the final digest agree, and the
// category set covers the high-frequency tools observed in real streams
// (Read/Bash/Edit/Write/MCP/subagent); low-frequency tools (WebSearch/Cron/…)
// are omitted rather than reserved.
func (s *ProgressState) Summary() string {
	totals := categoryTotals(s.tools)
	var parts []string
	for _, cat := range []string{"read", "exec", "edit", "write", "mcp", "sub"} {
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
