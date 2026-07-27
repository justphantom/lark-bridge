package renderer

import (
	"strconv"
	"strings"
)

// maxExpandedSubagents bounds how many subagent detail blocks render inline;
// beyond it the zone folds to a todo-style status-count summary plus the
// most recent detail. Far smaller than maxExpandedTodos (10) because a
// subagent block spans 3-4 lines (type+title+meta+preview) vs a todo's 1 —
// 10 subagents would consume the card's whole element budget.
//
// See docs/subagent-rendering-design.md §4.6.1 for the threshold rationale.
const maxExpandedSubagents = 3

// maxSubagentPreviewRunes caps the preview shown on the card. The full
// output stays in the stream file / subagent child session for after-the-
// fact review; the card is a live dashboard, not a reading surface.
const maxSubagentPreviewRunes = 200

// SubagentInfo mirrors protocol.SubagentSummary at the renderer boundary
// (this package does not import protocol — the dispatcher converts at the
// boundary, same convention as GateInfo/TodoItem). All-string fields make a
// one-level copy a deep copy, so Clone copies the slice header only.
type SubagentInfo struct {
	Status       string // running|completed|failed
	TaskType     string // local_agent (claude) / agent (opencode)
	Type         string // subagent_type (explore/general-purpose/...)
	Title        string // stable task title
	Description  string // live action (claude task_progress only)
	ChildSession string // correlation key (task_id / child sessionId)
	Model        string
	DurationMs   int64
	ToolUses     int
	LastToolName string
	TotalTokens  int
	Preview      string // terminal: full output text (renderer truncates)
	OutputBytes  int
	Truncated    bool
}

// subagentRow is the mutable accumulator behind one subagent delegation.
// Fields are grouped: identity (type_/taskType/title/childID) is stable across
// the lifecycle; live (desc/model/durationMs/toolUses/lastTool/totalTokens)
// updates every task_progress; terminal (preview/outputBytes) lands on result.
type subagentRow struct {
	type_       string
	taskType    string
	title       string
	childID     string
	status      string // running | completed | failed
	desc        string
	model       string
	durationMs  int64
	toolUses    int
	lastTool    string
	totalTokens int
	preview     string
	outputBytes int
	truncated   bool
}

// AddSubagentUse records a subagent's running/progress state. A repeated
// call with the same ChildSession (claude task_id / opencode child session)
// updates the existing row in place rather than spawning a duplicate, so
// claude's task_started → task_progress ×N lifecycle folds into one entry
// even though description / cumulative usage drift across the ticks.
//
// Only the fields meaningful in the running phase are read: identity +
// live description + cumulative usage. Terminal fields (Preview/OutputBytes)
// are ignored here and applied by AddSubagentResult.
func (s *ProgressState) AddSubagentUse(info SubagentInfo) {
	if info.ChildSession != "" {
		for i := range s.subagents {
			if s.subagents[i].childID == info.ChildSession {
				applySubagentUse(&s.subagents[i], info)
				return
			}
		}
	}
	row := subagentRow{status: "running"}
	applySubagentUse(&row, info)
	s.subagents = append(s.subagents, row)
}

// applySubagentUse overwrites the running-phase fields of row from info.
// Identity fields (type_/title/childID) are kept stable once set so a
// progress tick that omits them (claude drops subagent_type on some lines)
// does not blank the row's header.
func applySubagentUse(row *subagentRow, info SubagentInfo) {
	if info.TaskType != "" {
		row.taskType = info.TaskType
	}
	if info.Type != "" {
		row.type_ = info.Type
	}
	if info.Title != "" {
		row.title = info.Title
	}
	if info.ChildSession != "" {
		row.childID = info.ChildSession
	}
	if info.Description != "" {
		row.desc = info.Description
	}
	if info.Model != "" {
		row.model = info.Model
	}
	if info.ToolUses > 0 {
		row.toolUses = info.ToolUses
	}
	if info.DurationMs > 0 {
		row.durationMs = info.DurationMs
	}
	if info.TotalTokens > 0 {
		row.totalTokens = info.TotalTokens
	}
	if info.LastToolName != "" {
		row.lastTool = info.LastToolName
	}
	// status stays running; terminal transition is AddSubagentResult's job.
}

// AddSubagentResult records a subagent's terminal state, carrying the
// preview/output-bytes the zone renders on completion. When ChildSession
// matches an existing running row (claude task_started→notification), the
// row is closed in place. When no prior row exists (opencode's single
// completed task event), a fresh terminal row is appended.
func (s *ProgressState) AddSubagentResult(info SubagentInfo) {
	status := info.Status
	if status == "" {
		status = "completed"
	}
	if info.ChildSession != "" {
		for i := range s.subagents {
			if s.subagents[i].childID == info.ChildSession {
				applySubagentUse(&s.subagents[i], info)
				s.subagents[i].status = status
				s.subagents[i].preview = info.Preview
				s.subagents[i].outputBytes = info.OutputBytes
				return
			}
		}
	}
	row := subagentRow{status: status}
	applySubagentUse(&row, info)
	row.preview = info.Preview
	row.outputBytes = info.OutputBytes
	s.subagents = append(s.subagents, row)
}

// renderSubagentZone builds the subagent zone markdown, or "" when no
// subagents exist. Layout (see docs/subagent-rendering-design.md §4.2/§4.6.1):
//
//   - ≤ maxExpandedSubagents active (running+completed): each renders as a
//     multi-line block (type + status + duration, title, live desc or
//     preview, meta line).
//   - > maxExpandedSubagents active: folds to a todo-style status-count
//     summary plus the single most recent active detail, so the user still
//     sees one concrete delegation rather than only a number.
//   - failed subagents are ALWAYS listed verbatim (never folded into the
//     count), mirroring the errored-zone philosophy that each failure's
//     reason matters.
//
// Returns "" when the slice is empty so the caller omits the zone entirely.
func renderSubagentZone(rows []subagentRow) string {
	if len(rows) == 0 {
		return ""
	}
	var active, failed []subagentRow
	for _, r := range rows {
		if r.status == "failed" {
			failed = append(failed, r)
		} else {
			active = append(active, r)
		}
	}

	var lines []string
	if len(active) <= maxExpandedSubagents {
		for _, r := range active {
			lines = append(lines, formatSubagentBlock(r))
		}
	} else {
		lines = append(lines, formatSubagentSummary(active))
		// Append the most recent active detail so the zone never collapses
		// to just a number — the latest delegation stays visible.
		lines = append(lines, formatSubagentCompact(active[len(active)-1]))
	}
	// Failed subagents always render in full, after the active block.
	for _, r := range failed {
		lines = append(lines, formatSubagentBlock(r))
	}
	return strings.Join(lines, "\n")
}

// formatSubagentBlock renders one subagent as a multi-line markdown block.
// Running rows show the live description; terminal rows show the capped
// preview. The meta line carries child session + model + duration + tool
// count, each segment omitted when empty (omitempty at the field level
// keeps the line compact for backends that lack the field).
func formatSubagentBlock(r subagentRow) string {
	var b strings.Builder
	// Header: icon + type + status + duration.
	b.WriteString(subagentIcon(r.status))
	b.WriteString(" ")
	if r.type_ != "" {
		b.WriteString(r.type_)
	} else {
		b.WriteString("子代理")
	}
	b.WriteString(" · ")
	b.WriteString(subagentStatusLabel(r.status))
	if dur := formatSubagentDuration(r.durationMs); dur != "" {
		b.WriteString(" · ")
		b.WriteString(dur)
	}
	b.WriteString("\n")

	// Title (bold) — stable across the lifecycle.
	if r.title != "" {
		b.WriteString("**")
		b.WriteString(truncateRunes(r.title, maxToolDescLen))
		b.WriteString("**\n")
	}
	// Live description (running) or capped preview (terminal).
	if r.status == "running" && r.desc != "" {
		b.WriteString("> ")
		b.WriteString(truncateRunes(r.desc, maxToolDescLen))
		b.WriteString("\n")
	} else if r.preview != "" {
		b.WriteString("> ")
		b.WriteString(truncateRunes(r.preview, maxSubagentPreviewRunes))
		b.WriteString("\n")
	}
	// Meta line: child session + model + tool count + output size.
	meta := formatSubagentMeta(r)
	if meta != "" {
		b.WriteString(meta)
		b.WriteString("\n")
	}
	// strings.Join trims the trailing newline per block.
	return strings.TrimRight(b.String(), "\n")
}

// formatSubagentCompact renders one subagent as a single line for the
// folded view (the "most recent detail" tail). Compact form: icon + type +
// title + status + duration + size, all on one line.
func formatSubagentCompact(r subagentRow) string {
	var parts []string
	parts = append(parts, subagentIcon(r.status))
	if r.type_ != "" {
		parts = append(parts, r.type_)
	} else {
		parts = append(parts, "子代理")
	}
	if r.title != "" {
		parts = append(parts, truncateRunes(r.title, maxToolDescLen))
	}
	parts = append(parts, subagentStatusLabel(r.status))
	if dur := formatSubagentDuration(r.durationMs); dur != "" {
		parts = append(parts, dur)
	}
	if sz := formatSubagentSize(r.outputBytes); sz != "" {
		parts = append(parts, sz)
	}
	return strings.Join(parts, " · ")
}

// formatSubagentSummary renders the folded status count, todo-style:
// "🧪 子代理 N · ✅a ⏳b" where N=total active, a=completed, b=running.
func formatSubagentSummary(active []subagentRow) string {
	var completed, running int
	for _, r := range active {
		switch r.status {
		case "running":
			running++
		default:
			completed++
		}
	}
	var parts []string
	parts = append(parts, "🧪 子代理 "+strconv.Itoa(len(active)))
	if completed > 0 {
		parts = append(parts, "✅"+strconv.Itoa(completed))
	}
	if running > 0 {
		parts = append(parts, "⏳"+strconv.Itoa(running))
	}
	return strings.Join(parts, " · ")
}

// formatSubagentMeta composes the meta line: 🆔 childID · model · N步 · size.
// Returns "" when no segment is present so the caller omits the line.
func formatSubagentMeta(r subagentRow) string {
	var parts []string
	if r.childID != "" {
		id := r.childID
		if len([]rune(id)) > 12 {
			id = string([]rune(id)[:12])
		}
		parts = append(parts, "🆔 "+id)
	}
	if r.model != "" {
		parts = append(parts, r.model)
	}
	if r.toolUses > 0 {
		parts = append(parts, strconv.Itoa(r.toolUses)+"步")
	}
	if r.lastTool != "" && r.status == "running" {
		parts = append(parts, "最近 "+truncateRunes(r.lastTool, maxToolNameLen))
	}
	if sz := formatSubagentSize(r.outputBytes); sz != "" {
		parts = append(parts, sz)
	}
	if r.truncated {
		parts = append(parts, "已截断")
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " · ")
}

// subagentIcon picks the row's leading emoji by status. failed uses ❌
// (not ✘) to read as a hard failure at a glance, distinct from the
// leaf-tool error icon ❌ but heavier weighted via the red label below.
func subagentIcon(status string) string {
	switch status {
	case "running":
		return "🧪"
	case "failed":
		return "❌"
	default:
		return "🧪"
	}
}

// subagentStatusLabel renders the status word for the header line.
// "completed" maps to ✅ to keep the icon-less header still scannable.
func subagentStatusLabel(status string) string {
	switch status {
	case "running":
		return "运行中"
	case "failed":
		return "失败"
	default:
		return "✅"
	}
}

// formatSubagentDuration renders durationMs as a compact "Ns" / "Nm" / "Nh".
// Returns "" for zero so the meta line omits the segment when the backend
// lacked timing (claude total_tokens is often 0 for the same reason).
func formatSubagentDuration(ms int64) string {
	if ms <= 0 {
		return ""
	}
	sec := ms / 1000
	if sec < 60 {
		return strconv.FormatInt(sec, 10) + "s"
	}
	if sec < 3600 {
		return strconv.FormatInt(sec/60, 10) + "m"
	}
	return strconv.FormatInt(sec/3600, 10) + "h"
}

// formatSubagentSize renders outputBytes as a compact "NKB" / "NMB" hint.
// Returns "" for zero (a subagent whose output never landed, e.g. an
// in-flight running row, omits the segment).
func formatSubagentSize(bytes int) string {
	if bytes <= 0 {
		return ""
	}
	if bytes < 1024 {
		return strconv.Itoa(bytes) + "B"
	}
	if bytes < 1024*1024 {
		return strconv.FormatFloat(float64(bytes)/1024, 'f', 1, 64) + "KB"
	}
	return strconv.FormatFloat(float64(bytes)/(1024*1024), 'f', 1, 64) + "MB"
}
