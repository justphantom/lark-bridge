package claudebridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/justphantom/lark-bridge/internal/claude"
)

// isLocalAgentKind reports whether kind is claude's true AI subagent class.
// local_bash (background shell wrapped as a task) returns false so the
// renderer keeps it as a leaf Bash row; only local_agent gets the dedicated
// subagent zone (see docs/subagent-rendering-design.md §6.4).
func isLocalAgentKind(kind string) bool { return kind == "local_agent" }

// extractTaskLastToolName parses last_tool_name from a task_progress system
// line's raw JSON. The vendored claude SDK (internal/claude, read-only) does
// not surface this field on Event — only the usage triple — so the bridge
// decodes it from Event.Raw, which is explicitly retained "for parsing
// sub-fields by the caller". Returns "" for non-progress lines or absent
// fields; the renderer omits an empty LastToolName segment.
func extractTaskLastToolName(raw string) string {
	if raw == "" {
		return ""
	}
	var v struct {
		LastToolName string `json:"last_tool_name"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil {
		return ""
	}
	return v.LastToolName
}

// taskRunningMeta caches the latest task_progress fields per task_id so
// task_notification can carry the final cumulative usage/tool even though
// claude's notification line itself drops usage. Zero value is valid (a
// subagent that never sent progress).
type taskRunningMeta struct {
	desc        string
	toolUses    int
	durationMs  int64
	totalTokens int
	lastTool    string
}

// taskToolName renders the subagent as a tool-row name. taskKind discriminates
// upstream's task classes: "local_bash" is a Bash subprocess (rendered "Shell",
// not an AI subagent) while "local_agent" is a true agent delegation. The
// subagentType (e.g. "Explore", "general-purpose") is shown for local_agent so
// the row reads "Explore Agent" and stays distinct from a plain "Explore" tool.
// When both are empty (older streams / notification rows where upstream omits
// them) the generic "Agent" keeps prior behaviour.
func taskToolName(subagentType, taskKind string) string {
	if taskKind == "local_bash" {
		return "Shell"
	}
	if subagentType != "" {
		return subagentType + " Agent"
	}
	return "Agent"
}

// taskProgressDesc composes the subagent row description: the live action
// (progress tick) or terminal title (notification) plus cumulative usage. It
// rides on the ToolUse/ToolResult Input so the usage shows in the row
// description rather than the tool output (the progress card omits output).
func taskProgressDesc(ev claude.Event) string {
	desc := ev.TaskDesc
	stats := taskUsageStats(ev)
	if stats == "" {
		return desc
	}
	if desc == "" {
		return stats
	}
	return desc + " · " + stats
}

// taskUsageStats formats the cumulative subagent usage as a compact "N步 · Xs ·
// Mk tokens" string, omitting any zero fields.
func taskUsageStats(ev claude.Event) string {
	var parts []string
	if steps := ev.TaskSteps; steps > 0 {
		parts = append(parts, strconv.Itoa(steps)+"步")
	}
	if ms := ev.TaskMs; ms > 0 {
		parts = append(parts, fmt.Sprintf("%.1fs", float64(ms)/1000))
	}
	if tokens := ev.TaskTokens; tokens > 0 {
		parts = append(parts, formatTokenCount(tokens))
	}
	return strings.Join(parts, " · ")
}

// formatTokenCount renders a token count compactly (k for thousands).
func formatTokenCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%.0fk tokens", float64(n)/1000)
	}
	return strconv.Itoa(n) + " tokens"
}
