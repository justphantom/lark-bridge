package ompbridge

import (
	"encoding/json"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// todoWriteInput is the JSON shape a todowrite-style tool receives:
//
//	{"todos":[{"content":"…","status":"pending|in_progress|completed|cancelled","priority":"high|medium|low"}]}
//
// The protocol.TodoItem shape matches, so each entry decodes straight into a
// protocol.TodoItem without remapping. Kept for forward-compat: omp's todo
// tool (if any) is treated as a plain tool row in v1; this helper lets a
// future change route it to TypeTodo without touching stream_loop's tool
// case.
type todoWriteInput struct {
	Todos []protocol.TodoItem `json:"todos"`
}

// parseTodoItems decodes a todowrite tool input string into the protocol's
// TodoItem slice. Returns (items, true) on a usable list; (nil, false) when
// input is not JSON, has no todos array, or the array is empty — the caller
// falls back to the generic TypeToolResult path so a degraded event still
// surfaces on the card instead of vanishing.
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
