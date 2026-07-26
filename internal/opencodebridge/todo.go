package opencodebridge

import (
	"encoding/json"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// todoWriteInput is the JSON shape opencode's todowrite tool receives:
//
//	{"todos":[{"content":"…","status":"pending|in_progress|completed|cancelled","priority":"high|medium|low"}]}
//
// opencode's schema (packages/opencode/src/session/todo.ts → SessionTodo.Info)
// and lark-bridge's protocol.TodoItem share this exact field set, so each
// entry decodes straight into a protocol.TodoItem without remapping.
type todoWriteInput struct {
	Todos []protocol.TodoItem `json:"todos"`
}

// parseTodoItems decodes a todowrite tool input string into the protocol's
// TodoItem slice. Returns (items, true) on a usable list; (nil, false) when
// input is not JSON, has no todos array, or the array is empty — the caller
// (stream_loop) falls back to the generic TypeToolResult path so a degraded
// todowrite event still surfaces on the card instead of vanishing.
//
// Status / priority values are NOT re-validated here: opencode's own schema
// already constrains them to the enum protocol.TodoItem expects, and the
// frontend's Control.Validate runs the canonical check on the wire.
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
