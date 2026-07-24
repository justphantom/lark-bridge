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

// truncateRunes caps s to maxRunes runes, appending "…" if truncated.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// maxExpandedTodos bounds how many todo items render as detail rows; beyond it
// the zone folds to a summary, since todo lists routinely hit 20+ items and a
// Feishu card caps at ~50 elements.
const maxExpandedTodos = 10

// renderTodoZone renders the todo list as markdown. Up to maxExpandedTodos
// items each get a status-prefixed line; beyond that it collapses to
// "清单 N/M · ✅a ⏳b ⬜c ✘d" (M=total, N=settled=completed+cancelled).
// cancelled items are kept but greyed — they carry audit value.
func renderTodoZone(todos []TodoItem) string {
	if len(todos) > maxExpandedTodos {
		var done, inProg, pending, cancelled int
		for _, td := range todos {
			switch td.Status {
			case "completed":
				done++
			case "in_progress":
				inProg++
			case "cancelled":
				cancelled++
			default:
				pending++
			}
		}
		settled := done + cancelled
		return "清单 " + strconv.Itoa(settled) + "/" + strconv.Itoa(len(todos)) +
			" · ✅" + strconv.Itoa(done) + " ⏳" + strconv.Itoa(inProg) +
			" ⬜" + strconv.Itoa(pending) + " ✘" + strconv.Itoa(cancelled)
	}
	lines := make([]string, 0, len(todos))
	for _, td := range todos {
		icon := "⬜"
		switch td.Status {
		case "completed":
			icon = "✅"
		case "in_progress":
			icon = "⏳"
		case "cancelled":
			icon = "✘"
		}
		line := icon + " " + td.Content
		if td.Status == "cancelled" {
			line = "<font color=\"grey\">" + line + "</font>"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
