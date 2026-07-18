// Package miniclient wraps the miniagent subprocess: it forks the CLI,
// pipes the prompt via stdin, and pumps stdout NDJSON events into a channel.
// It is the miniagent analogue of internal/claude.Client.
package miniclient

import "encoding/json"

// Event kind constants.
const (
	KindToolUse    = "tool_use"
	KindToolResult = "tool_result"
	KindResult     = "result"
	KindError      = "error"
)

// Event is one parsed stream-json line from miniagent's stdout. A
// terminal event (KindResult or KindError) is always emitted last; the
// pump goroutine closes the channel after it.
type Event struct {
	Kind  string // tool_use | tool_result | result | error
	Name  string // tool name (tool_use/tool_result only)
	Input string // tool call input args JSON (tool_use/tool_result)
	// Output is the tool result text (tool_result only).
	Output  string
	IsError bool // tool_result: the tool failed; error: always true

	// result event fields.
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	Steps        int

	// error event fields.
	Message string

	// Derived: true for KindResult and KindError.
	IsTerminal bool
}

// rawEvent mirrors the JSON shape miniagent writes (internal/miniagent
// .streamEvent). Kept unexported: callers interact via Event.
type rawEvent struct {
	Type         string `json:"type"`
	Name         string `json:"name,omitempty"`
	Input        string `json:"input,omitempty"`
	Output       string `json:"output,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`
	Text         string `json:"text,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	Steps        int    `json:"steps,omitempty"`
	Message      string `json:"message,omitempty"`
}

// parseEvent decodes one NDJSON line into an Event. Returns ok=false on
// malformed JSON (the pump skips those lines).
func parseEvent(line []byte) (Event, bool) {
	var raw rawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}
	ev := Event{
		Kind:         raw.Type,
		Name:         raw.Name,
		Input:        raw.Input,
		Output:       raw.Output,
		IsError:      raw.IsError,
		Text:         raw.Text,
		Model:        raw.Model,
		InputTokens:  raw.InputTokens,
		OutputTokens: raw.OutputTokens,
		Steps:        raw.Steps,
		Message:      raw.Message,
	}
	switch raw.Type {
	case KindResult, KindError:
		ev.IsTerminal = true
		if raw.Type == KindError {
			ev.IsError = true // error events are always isError regardless of the field
		}
	}
	return ev, true
}
