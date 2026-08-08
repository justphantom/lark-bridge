package miniagent

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// todoToolNames is the set of miniagent's todo tools. create/update carry a
// JSON item in their tool_result output; list carries a plain-text roster.
// All three are suppressed from the tool-row zones and folded into the
// progress card's todo zone via TypeTodo snapshots, mirroring how claude's
// TodoWrite is handled.
var todoToolNames = map[string]bool{
	"todo_create": true,
	"todo_update": true,
	"todo_list":   true,
}

// isTodoTool reports whether name is one of miniagent's todo tools.
func isTodoTool(name string) bool { return todoToolNames[name] }

// todoAccum accumulates todo items from a single miniagent turn. It is created
// as a local in runViaCLI, so it dies with the turn — no cross-turn leakage
// and no manual cleanup. miniagent's todo state is per-Run in-memory (each
// buildTools call makes a fresh TodoList), so a one-turn accum is the correct
// scope.
//
// applyResult decodes a create/update JSON output
// ({"id":1,"subject":"…","status":"pending"}) and merges it by id (upserting
// into the ordered slice). applyList parses a list output
// (#1 [pending] subject) as a full-state sync. snapshot returns the current
// items in id order for a TypeTodo payload.
type todoAccum struct {
	mu    sync.Mutex
	items []protocol.TodoItem // ordered by miniagent-assigned id (ascending)
}

func newTodoAccum() *todoAccum { return &todoAccum{} }

// miniagentTodoItem is the JSON shape returned by todo_create / todo_update.
// Fields align with miniagent's internal tool_todo.go TodoItem.
type miniagentTodoItem struct {
	ID      int    `json:"id"`
	Subject string `json:"subject"`
	Status  string `json:"status"`
}

// applyResult merges one create/update result into the accum by id (upsert).
// Returns true when the JSON decoded to a usable item (non-zero id). The
// subject maps to protocol.TodoItem.Content (the card field); miniagent has
// no priority, so Priority is left empty.
func (a *todoAccum) applyResult(output string) bool {
	var mi miniagentTodoItem
	if err := json.Unmarshal([]byte(output), &mi); err != nil || mi.ID == 0 {
		return false
	}
	item := protocol.TodoItem{Content: mi.Subject, Status: mi.Status}
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.items {
		// protocol.TodoItem has no id field, so identity is by content.
		// An update to an existing item keeps its slot; a new item appends.
		if a.items[i].Content == item.Content {
			a.items[i] = item
			return true
		}
	}
	a.items = append(a.items, item)
	return true
}

// todoListItemPattern matches one line of a todo_list output:
// "#1 [pending] 写测试".
var todoListItemPattern = regexp.MustCompile(`^#(\d+)\s+\[(\w+)\]\s+(.+)$`)

// applyList parses a todo_list plain-text roster and replaces the entire
// accum with the parsed items (full-state sync, preserving id order).
// Returns true when at least one item was parsed; false for "（无任务）" or
// unparseable output.
func (a *todoAccum) applyList(output string) bool {
	var parsed []protocol.TodoItem
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		m := todoListItemPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		parsed = append(parsed, protocol.TodoItem{Content: m[3], Status: m[2]})
	}
	if len(parsed) == 0 {
		return false
	}
	a.mu.Lock()
	a.items = parsed
	a.mu.Unlock()
	return true
}

// snapshot returns a copy of the current items in their accumulated order.
func (a *todoAccum) snapshot() []protocol.TodoItem {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]protocol.TodoItem, len(a.items))
	copy(out, a.items)
	return out
}

// emitTodoResult handles a todo tool_result by folding the result into the
// per-turn accumulator and emitting a TypeTodo snapshot (not a TypeToolResult).
// todo_create / todo_update carry single-item JSON (merged by content);
// todo_list carries a plain-text roster (full-state sync). A non-empty
// snapshot always emits TypeTodo so the card's todo zone reflects the latest
// state. When the result is unparseable (e.g. an error output), no TypeTodo is
// emitted — the card keeps its last-known state rather than flashing empty.
func (h *Handler) emitTodoResult(chatID, promptID string, ev miniclient.Event, todos *todoAccum) {
	if ev.IsError {
		return
	}
	switch ev.Name {
	case "todo_list":
		todos.applyList(ev.Output)
	default: // todo_create / todo_update
		todos.applyResult(ev.Output)
	}
	items := todos.snapshot()
	if len(items) == 0 {
		return
	}
	h.sendCtrl(&protocol.Control{
		Type:   protocol.TypeTodo,
		ChatID: chatID,
		Todo:   &protocol.TodoPayload{Todos: items},
	})
}
