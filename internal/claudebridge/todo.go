package claudebridge

import (
	"encoding/json"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// todoWriteInput is the JSON shape claude's TodoWrite tool receives:
//
//	{"todos":[{"content":"…","status":"pending|in_progress|completed|cancelled","priority":"high|medium|low"}]}
//
// Identical to opencode's todowrite and to protocol.TodoItem, but kept as a
// per-package copy rather than shared: claudebridge and opencodebridge are
// sibling packages with no dependency edge between them, and the parse is
// small enough (<3 sites) that promoting it to bridgebase would be premature
// per the project's abstraction budget.
type todoWriteInput struct {
	Todos []protocol.TodoItem `json:"todos"`
}

// parseTodoItems decodes a TodoWrite tool input string into protocol.TodoItem
// slice. Returns (items, true) on a usable list; (nil, false) on empty input,
// non-JSON, or empty array — the caller (stream_loop) falls back to the
// generic TypeToolResult path so a degraded event still surfaces.
func parseTodoItems(input string) ([]protocol.TodoItem, bool) {
	if input == "" || input == "{}" {
		return nil, false
	}
	var parsed todoWriteInput
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil, false
	}
	if len(parsed.Todos) == 0 {
		return nil, false
	}
	return parsed.Todos, true
}
