package miniagent

import (
	"encoding/json"
	"io"
)

// streamEvent is one line of stream-json output (NDJSON). Each line is a
// self-contained JSON object written to stdout. The CLI emits these as the
// loop runs; a downstream consumer (minibridge, shell pipe, or a human
// reading verbose output) reads them line by line.
type streamEvent struct {
	Type string `json:"type"` // tool_use | tool_result | result | error

	// tool_use / tool_result fields
	Name    string `json:"name,omitempty"`
	Input   string `json:"input,omitempty"`
	Output  string `json:"output,omitempty"`
	IsError bool   `json:"is_error,omitempty"`

	// result fields
	Text         string `json:"text,omitempty"`
	Model        string `json:"model,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	Steps        int    `json:"steps,omitempty"`

	// error fields
	Message string `json:"message,omitempty"`
}

// StreamEmitFunc returns an EmitFunc that writes Signal events as NDJSON
// lines to w. When verbose is false, only tool_use signals are emitted
// (tool_result is suppressed to reduce noise; the final result event always
// prints regardless). When verbose is true, all signals print.
func StreamEmitFunc(w io.Writer, verbose bool) EmitFunc {
	enc := json.NewEncoder(w)
	return func(sig Signal) {
		if !verbose && sig.Kind != SignalToolUse {
			return
		}
		_ = enc.Encode(signalToStreamEvent(sig))
	}
}

// EmitResult writes the terminal result event to w.
func EmitResult(w io.Writer, result Result, model string) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(streamEvent{
		Type:         "result",
		Text:         result.Text,
		Model:        model,
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
		Steps:        result.Steps,
	})
}

// EmitError writes the terminal error event to w.
func EmitError(w io.Writer, msg string) {
	enc := json.NewEncoder(w)
	_ = enc.Encode(streamEvent{
		Type:    "error",
		Message: msg,
	})
}

func signalToStreamEvent(sig Signal) streamEvent {
	switch sig.Kind {
	case SignalToolUse:
		return streamEvent{Type: "tool_use", Name: sig.Name, Input: sig.Input}
	case SignalToolResult:
		return streamEvent{Type: "tool_result", Name: sig.Name, Input: sig.Input, Output: sig.Output, IsError: sig.IsError}
	default:
		return streamEvent{Type: string(sig.Kind), Name: sig.Name}
	}
}
