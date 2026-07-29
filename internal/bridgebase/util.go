package bridgebase

import (
	"encoding/json"
	"strings"

	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// MaxDebugTextLen caps the preview length used in debug logs.
const MaxDebugTextLen = 200

// NonEmpty returns s when its trimmed form is non-empty, fallback otherwise.
// Used by all CLI bridges' error-event paths to give a meaningful message when
// the CLI emits an empty error text.
func NonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// TruncateForDebug returns a string for debug logging: optionally redacted
// (replaced wholesale) and always truncated to MaxDebugTextLen.
func TruncateForDebug(s string, redact bool) string {
	if redact {
		return "<redacted>"
	}
	return strutil.Truncate(s, MaxDebugTextLen)
}

// FirstNonEmpty returns the first non-empty string, or "" if all are empty.
// Used by claude bridge for model fallback (stream model → user-pinned spec).
func FirstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// ResolveModel picks the model name for the result card. The CLI's NDJSON
// stream may carry no model name on the events the bridge consumes, so when
// neither the stream-supplied model nor the user's modelSpec is set, fallback
// is used (e.g. "omp" / "opencode"). The first parameter is the stream model;
// it currently is always passed empty by opencode/omp (kept in the signature
// for forward-compat when a CLI starts emitting it).
func ResolveModel(streamModel, spec, fallback string) string {
	if streamModel != "" {
		return streamModel
	}
	if spec != "" {
		return spec
	}
	return fallback
}

// todoWriteInput is the JSON shape a todowrite-style tool receives:
//
//	{"todos":[{"content":"…","status":"pending|in_progress|completed|cancelled","priority":"high|medium|low"}]}
//
// protocol.TodoItem matches that shape, so each entry decodes straight into it
// without remapping. Kept unexported: only ParseTodoItems is the public face.
type todoWriteInput struct {
	Todos []protocol.TodoItem `json:"todos"`
}

// ParseTodoItems decodes a todowrite tool input string into the protocol's
// TodoItem slice. Returns (items, true) on a usable list; (nil, false) when
// input is not JSON, has no todos array, or the array is empty — the caller
// falls back to the generic TypeToolResult path so a degraded event still
// surfaces on the card instead of vanishing.
func ParseTodoItems(input string) ([]protocol.TodoItem, bool) {
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
