package renderer

import (
	"strconv"
	"strings"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
)

// maxCompletedTools bounds how many completed-tool detail rows are shown.
// Older ones are folded into the grouped summary ("… 另完成 读取 N · 执行 N")
// so the card does not grow unbounded on long multi-round agent tasks. 3
// keeps the recent-actions window short — the grouped summary carries the
// scale, the window carries only "what just happened".
const maxCompletedTools = 3

// maxRunningTools bounds how many running-tool rows are shown. When
// exceeded, older running rows are collapsed into a "... 及 N 个运行中"
// summary. In practice concurrent tools rarely exceed 3-4, but this
// prevents a subagent storm from exploding the card height.
const maxRunningTools = 5

// maxToolOutputLen caps the error excerpt shown for a failed tool row. 50
// runes is enough to convey the failure reason (permission denied, exit 1,
// timeout) without letting a verbose stack trace crowd the error zone.
const maxToolOutputLen = 50

// toolRow tracks one tool invocation through its lifecycle.
type toolRow struct {
	name       string
	desc       string // human-readable summary (file path, command, etc.)
	status     string // running | completed | error
	output     string // truncated error excerpt, shown only when status == error
	count      int    // number of same name+desc calls collapsed into this row
	isSubagent bool   // claude "<Type> Agent" row or opencode "task" tool
	taskID     string // claude subagent identifier; empty for leaf tools/opencode
}

// ProgressState accumulates the streaming pieces of one prompt so each
// progress update renders the whole card.
type ProgressState struct {
	tools     []toolRow
	todos     []TodoItem
	stepCount int
}

// TodoItem is the renderer's own todo entry (mirrors protocol.TodoItem but
// keeps this package free of a protocol import — the dispatcher converts at
// the boundary). All-string fields make a one-level copy a deep copy.
type TodoItem struct {
	Content  string
	Status   string // pending|in_progress|completed|cancelled
	Priority string
}

// NewProgressState builds an empty state.
func NewProgressState() *ProgressState { return &ProgressState{} }

// AddToolUse records a tool invocation start. A repeated call with the same
// name+desc collapses into the existing row (incrementing count) rather than
// spawning a duplicate; this keeps consecutive identical calls (e.g. reading
// the same file twice) visible as "Read: /x ×2" instead of two rows.
// isSubagent is the backend's authoritative flag (not a name-based guess).
// For subagents with a non-empty taskID (claude), matching is by taskID so the
// row folds its started/progress/notification lifecycle into one entry even
// though name/desc drift across it; leaf tools (and opencode subagents, which
// carry no taskID) keep the legacy name+desc match.
func (s *ProgressState) AddToolUse(name, desc string, isSubagent bool, taskID string) {
	// Normalize tool name: first letter upper-case (or mcp:<tool>).
	name = normalizeToolName(name)
	if isSubagent && taskID != "" {
		for i := range s.tools {
			if s.tools[i].isSubagent && s.tools[i].taskID == taskID {
				s.tools[i].status = "running"
				s.tools[i].desc = desc
				s.tools[i].count++
				s.tools[i].name = name
				return
			}
		}
		s.tools = append(s.tools, toolRow{name: name, desc: desc, status: "running", count: 1, isSubagent: true, taskID: taskID})
		return
	}
	for i := range s.tools {
		if s.tools[i].name == name && s.tools[i].desc == desc {
			s.tools[i].status = "running"
			s.tools[i].count++
			s.tools[i].isSubagent = isSubagent
			return
		}
	}
	s.tools = append(s.tools, toolRow{name: name, desc: desc, status: "running", count: 1, isSubagent: isSubagent})
}

// AddToolResult records a tool completion. desc carries the input summary so a
// result-only event (no prior ToolUse, e.g. opencode's single completed line)
// still renders "Read: /path" when it appends a fresh row. When it matches an
// existing running row, that row keeps its own desc. Matching prefers the most
// recent running row of the same name. For subagents with a non-empty taskID
// (claude), the row is closed by taskID so the notification reaches the exact
// subagent that started — not merely the most-recent same-name running row
// (which under concurrency closes the wrong one). If that row was already
// collapsed out by maxRunningTools the result is accepted as an orphan
// completed entry rather than retroactively resizing the card.
func (s *ProgressState) AddToolResult(name, desc, output string, isError, isSubagent bool, taskID string) {
	name = normalizeToolName(name)
	status := "completed"
	if isError {
		status = "error"
	}
	preview := truncateOutput(output)
	if isSubagent && taskID != "" {
		for i := range s.tools {
			if s.tools[i].isSubagent && s.tools[i].taskID == taskID {
				s.tools[i].status = status
				s.tools[i].output = preview
				if desc != "" && desc != s.tools[i].desc {
					s.tools[i].desc = desc
				}
				return
			}
		}
		s.tools = append(s.tools, toolRow{name: name, desc: desc, status: status, output: preview, count: 1, isSubagent: true, taskID: taskID})
		return
	}
	for i := len(s.tools) - 1; i >= 0; i-- {
		if s.tools[i].name == name && s.tools[i].status == "running" {
			s.tools[i].status = status
			s.tools[i].output = preview
			s.tools[i].isSubagent = isSubagent
			// A terminal description (e.g. a subagent's notification summary
			// with cumulative usage) supersedes the live progress description
			// the row was showing while running.
			if desc != "" && desc != s.tools[i].desc {
				s.tools[i].desc = desc
			}
			return
		}
	}
	// No matching running tool — append as completed, using the input
	// summary as the row's description.
	s.tools = append(s.tools, toolRow{name: name, desc: desc, status: status, output: preview, count: 1, isSubagent: isSubagent})
}

// AddProgress counts a step.
func (s *ProgressState) AddProgress() {
	s.stepCount++
}

// AddTodo replaces the whole todo list. The backend sends the complete list on
// every todo_updated, so each call overwrites (not appends to) the prior state.
func (s *ProgressState) AddTodo(items []TodoItem) {
	s.todos = append(s.todos[:0], items...)
}

// Render produces the progress card JSON.
func (s *ProgressState) Render(header cardkit.HeaderInfo, footer cardkit.FooterInfo) ([]byte, error) {
	header.Template = "blue"
	if header.Title == "" {
		header.Title = "处理中"
	}
	// Enrich title with step + completed-tool count. Running tools are shown
	// in their own zone and excluded from the title count so the title tracks
	// settled actions (completed + errored), not the volatile in-flight set.
	if s.stepCount > 0 {
		header.Title += " · 第 " + strconv.Itoa(s.stepCount) + " 轮"
	}
	completedTitle := 0
	for _, t := range s.tools {
		if t.status != "running" {
			completedTitle++
		}
	}
	if completedTitle > 0 {
		header.Title += " · 已完成 " + strconv.Itoa(completedTitle)
	}

	// Each zone builds its own element (or nil when empty); appendZones
	// inserts an hr between adjacent non-empty zones so tools / text don't
	// run together when several are present.
	var zones []cardkit.Element

	// Tools split into three zones (running → completed → error). Error rows are separated out so the completed
	// zone's grouped summary counts only successes ("不含出错动作") and the
	// error zone can list failures verbatim with their excerpts.
	completed := 0
	var running, done, errored []string
	var doneRows []toolRow
	for _, t := range s.tools {
		line := formatToolLine(t)
		switch t.status {
		case "running":
			running = append(running, line)
		case "error":
			errored = append(errored, line)
		default:
			done = append(done, line)
			doneRows = append(doneRows, t)
			completed++
		}
	}

	// Zone 2: executing. Cap running tools — keep the most recent, collapse
	// older ones.
	var runningLines []string
	skipRunning := len(running) - maxRunningTools
	if skipRunning > 0 {
		runningLines = append(runningLines, "... 及 "+strconv.Itoa(skipRunning)+" 个运行中")
		runningLines = append(runningLines, running[skipRunning:]...)
	} else {
		runningLines = append(runningLines, running...)
	}
	if len(runningLines) > 0 {
		zones = append(zones, cardkit.MarkdownElement(strings.Join(runningLines, "\n")))
	}

	// Zone 3: completed. Show only the last N detail rows; group-summarise
	// the rest so the user sees scale + category mix during long tasks.
	var doneLines []string
	skip := completed - maxCompletedTools
	if skip > 0 {
		if summary := groupedSummary(categoryTotals(doneRows)); summary != "" {
			doneLines = append(doneLines, summary)
		} else {
			doneLines = append(doneLines, "... 及 "+strconv.Itoa(skip)+" 个已完成")
		}
		doneLines = append(doneLines, done[skip:]...)
	} else {
		doneLines = append(doneLines, done...)
	}
	if len(doneLines) > 0 {
		zones = append(zones, cardkit.MarkdownElement(strings.Join(doneLines, "\n")))
	}

	// Zone 3.5: todo list. Sits between completed (settled successes) and
	// errors (terminal failures) because todo is in-progress state. Kept as
	// its own zone so it never feeds categoryTotals (which counts tool rows).
	if len(s.todos) > 0 {
		zones = append(zones, cardkit.MarkdownElement(renderTodoZone(s.todos)))
	}

	// Zone 4: errors. Each failure is listed verbatim with its excerpt; no
	// collapsing — failures are rare and each one's reason matters.
	if len(errored) > 0 {
		zones = append(zones, cardkit.MarkdownElement(strings.Join(errored, "\n")))
	}

	return cardkit.Card(header, footer, appendZones(zones), nil)
}

// appendZones flattens zone elements into a single slice, inserting an hr
// divider between adjacent zones so the card reads as distinct sections.
func appendZones(zones []cardkit.Element) []cardkit.Element {
	var out []cardkit.Element
	for _, z := range zones {
		if len(out) > 0 {
			out = append(out, cardkit.HrElement())
		}
		out = append(out, z)
	}
	return out
}
