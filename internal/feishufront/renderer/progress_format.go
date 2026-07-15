package renderer

import (
	"strconv"
	"strings"
	"unicode"
)

// formatToolLine renders one tool row as a markdown line.
func formatToolLine(t toolRow) string {
	var icon, line string
	switch t.status {
	case "running":
		icon = "⏳"
	case "error":
		icon = "❌"
	default:
		icon = "✅"
	}
	line = icon + " " + t.name
	if t.desc != "" {
		line += ": " + t.desc
	}
	if t.count > 1 {
		line += " ×" + strconv.Itoa(t.count)
	}
	// The progress card shows actions, not their output. A failed tool is the
	// exception: its output is a short error excerpt so the user can see why.
	if t.output != "" && t.status == "error" {
		line += "\n   " + t.output
	}
	return line
}

// normalizeToolName renders a tool name for the card. MCP tools arrive as
// "mcp__<server>__<tool>" (40+ chars, wraps on mobile); shorten to
// "mcp:<tool>" keeping the mcp origin visible. Other names keep the legacy
// first-letter capitalisation (read → Read, bash → Bash).
func normalizeToolName(name string) string {
	if name == "" {
		return "?"
	}
	if strings.HasPrefix(name, "mcp__") {
		// "mcp__server__tool" → take the segment after the last "__".
		if i := strings.LastIndex(name, "__"); i >= 0 {
			tool := name[i+2:]
			if tool != "" {
				return "mcp:" + tool
			}
		}
		return "mcp"
	}
	r := []rune(name)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

// truncateOutput caps a failed tool's error excerpt to maxToolOutputLen runes.
func truncateOutput(s string) string {
	return truncateRunes(s, maxToolOutputLen)
}

// truncateRunes caps s to max runes, appending "…" if truncated.
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}
