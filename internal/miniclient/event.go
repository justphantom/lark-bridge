// Package miniclient wraps the miniagent subprocess: it forks the CLI,
// pipes the prompt via stdin, and pumps stdout NDJSON events into a channel.
// It is the miniagent analogue of internal/claude.Client.
package miniclient

import "encoding/json"

// Event kind constants.
const (
	KindToolUse = "tool_use"
	KindResult  = "result"
	KindError   = "error"

	// v2.0.0 event kinds. tool_result carries a tool's post-exec output (and,
	// for shell, its exit code). text_delta / reasoning_delta are streaming
	// increments emitted only when the CLI is started with -stream. Older
	// binaries never emit these; the pump simply never sees them.
	KindToolResult     = "tool_result"
	KindTextDelta      = "text_delta"
	KindReasoningDelta = "reasoning_delta"
)

// Finish reason constants reported on result events (miniagent v1.1.0+). Empty
// on older versions that predate the field.
const (
	FinishStop          = "stop"
	FinishMaxIterations = "max_iterations"
)

// Event is one parsed stream-json line from miniagent's stdout. A
// terminal event (KindResult or KindError) is always emitted last; the
// pump goroutine closes the channel after it.
type Event struct {
	Kind  string // tool_use | tool_result | text_delta | reasoning_delta | result | error
	Name  string // tool name (tool_use / tool_result)
	Input string // tool call input args JSON (tool_use only)

	// result event fields.
	Text         string
	Model        string
	InputTokens  int
	OutputTokens int
	Steps        int
	// Finish is the termination reason miniagent v1.1.0+ reports on the result
	// event: "stop" (normal) or "max_iterations" (hit the loop cap, Text empty).
	// Empty when the upstream CLI predates the field. Consumed by the miniagent
	// handler to set ResultPayload.Incomplete.
	Finish string

	// tool_result event fields (miniagent v2.0.0+). Output is the CLI-truncated
	// (2000-char) excerpt; the full result stays in the CLI's history for LLM
	// re-feed. IsError is the CLI's is_error flag. ExitCode is non-nil only for
	// the shell tool: v2.0.0 moved non-zero-exit success signalling from
	// IsError (now false) to ExitCode — nil means "not a shell result".
	Output    string
	Truncated bool
	IsError   bool
	ExitCode  *int

	// streaming delta event fields (text_delta / reasoning_delta, v2.0.0+ under
	// -stream). Step is the LLM-call index the delta belongs to; Text is the
	// incremental chunk (same JSON key as result.text).
	Step int

	// error event fields.
	Message string

	// Derived: true for KindResult and KindError.
	IsTerminal bool
}

// rawEvent mirrors the JSON shape miniagent writes (internal/miniagent
// .streamEvent / events.go). Kept unexported: callers interact via Event.
type rawEvent struct {
	Type         string `json:"type"`
	Name         string `json:"name,omitempty"`
	Input        string `json:"input,omitempty"`
	Text         string `json:"text,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	Steps        int    `json:"steps,omitempty"`
	// finish is miniagent v1.1.0+'s termination reason (stop / max_iterations),
	// absent on older versions and on non-result events.
	Finish  string `json:"finish,omitempty"`
	Message string `json:"message,omitempty"`

	// tool_result fields (v2.0.0+). exit_code is omitempty upstream and emitted
	// only for the shell tool, so a pointer preserves "absent" vs "zero".
	CallID    string `json:"call_id,omitempty"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	ExitCode  *int   `json:"exit_code,omitempty"`

	// streaming delta fields (v2.0.0+, -stream only).
	Step int `json:"step,omitempty"`
}

// parseEvent decodes one NDJSON line into an Event. Returns ok=false on
// malformed JSON (the pump skips those lines). An unrecognised type still
// returns ok=true with Kind set to the raw type and IsTerminal=false: the
// handler's switch falls through to default and ignores it, so a future
// upstream event type does not break the pump.
func parseEvent(line []byte) (Event, bool) {
	var raw rawEvent
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}
	ev := Event{
		Kind:         raw.Type,
		Name:         raw.Name,
		Input:        raw.Input,
		Text:         raw.Text,
		Model:        raw.Model,
		InputTokens:  raw.InputTokens,
		OutputTokens: raw.OutputTokens,
		Steps:        raw.Steps,
		Finish:       raw.Finish,
		Output:       raw.Output,
		Truncated:    raw.Truncated,
		IsError:      raw.IsError,
		ExitCode:     raw.ExitCode,
		Step:         raw.Step,
		Message:      raw.Message,
	}
	switch raw.Type {
	case KindResult, KindError:
		ev.IsTerminal = true
	}
	return ev, true
}
