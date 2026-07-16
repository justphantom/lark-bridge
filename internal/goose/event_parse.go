package goose

import (
	"encoding/json"
	"fmt"
	"strings"
)

// rawMessage is the on-wire shape of one goose stream-json line. goose wraps
// each streaming chunk as {type:"message", message:{content:[{...}]}} and the
// terminal as {type:"complete", total_tokens, input_tokens, output_tokens}.
// Fields beyond those mapped here are ignored (forward-compat).
//
// Verified empirically (goose 1.43.0): every message line carries exactly one
// content element, but the slice is traversed defensively in case a future
// build batches multiple blocks per line.
type rawMessage struct {
	Type    string `json:"type"`
	Message struct {
		Content []rawContent `json:"content,omitempty"`
	} `json:"message,omitempty"`
	// complete-only fields:
	TotalTokens  int `json:"total_tokens,omitempty"`
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

// rawContent is one element of message.content. The type discriminator selects
// which fields are populated.
type rawContent struct {
	Type       string         `json:"type"`
	Text       string         `json:"text,omitempty"`
	Thinking   string         `json:"thinking,omitempty"`
	ID         string         `json:"id,omitempty"`
	ToolCall   *toolCallRaw   `json:"toolCall,omitempty"`
	ToolResult *toolResultRaw `json:"toolResult,omitempty"`
}

// toolCallRaw is message.content[].toolCall for a toolRequest block.
// value.name is the tool name; value.arguments is the tool input (forwarded
// verbatim as JSON; the bridge does not parse it).
type toolCallRaw struct {
	Status string `json:"status"`
	Value  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	} `json:"value"`
}

// toolResultRaw is message.content[].toolResult for a toolResponse block.
// value.isError is the structural failure flag (absent ⇒ false).
type toolResultRaw struct {
	Status string `json:"status"`
	Value  struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content,omitempty"`
		IsError bool `json:"isError,omitempty"`
	} `json:"value"`
}

// parseLine decodes one stdout line into zero or more Events, and reports
// whether the line is terminal (the complete event). textAcc accumulates text
// chunks so a synthesized EventError at EOF (when no complete arrived) can
// still report what was produced.
//
// Returns (events, isTerminal, err). For goose:
//   - "complete" → one EventComplete (with usage), isTerminal=true
//   - "message" → 0+ events from content blocks (one per block), isTerminal=false
//   - unknown/blank → no events, no error (forward-compat)
func parseLine(line []byte, textAcc *strings.Builder) (events []Event, isTerminal bool, err error) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil, false, nil
	}
	var raw rawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, false, fmt.Errorf("goose line: %w", err)
	}
	switch raw.Type {
	case "complete":
		return []Event{{kind: EventComplete, inputTokens: raw.InputTokens, outputTokens: raw.OutputTokens}}, true, nil
	case "message":
		for _, ct := range raw.Message.Content {
			switch ev, ok := parseContent(ct, textAcc); {
			case ok:
				events = append(events, ev)
			}
		}
		return events, false, nil
	default:
		// Unknown top-level type: ignore at debug level in the caller.
		return nil, false, nil
	}
}

// parseContent maps one content block to an Event. Returns ok=false for
// unrecognized block types (caller skips them). textAcc is fed only by text
// blocks (the final-reply accumulator); thinking chunks are forwarded live and
// not accumulated, mirroring how the bridge streams TypeThinking separately.
func parseContent(ct rawContent, textAcc *strings.Builder) (Event, bool) {
	switch ct.Type {
	case "thinking":
		return Event{kind: EventThinking, text: ct.Thinking}, true
	case "text":
		if textAcc != nil {
			textAcc.WriteString(ct.Text)
		}
		return Event{kind: EventText, text: ct.Text}, true
	case "toolRequest":
		name := ""
		if ct.ToolCall != nil {
			name = ct.ToolCall.Value.Name
		}
		return Event{kind: EventToolUse, toolName: name, toolID: ct.ID}, true
	case "toolResponse":
		return parseToolResponse(ct), true
	default:
		return Event{}, false
	}
}

// parseToolResponse extracts the readable output + isError flag from a
// toolResponse block. goose nests the human-readable text inside
// value.content[].text (a slice, usually one element); concatenate non-empty
// text entries. isError comes from value.isError.
func parseToolResponse(ct rawContent) Event {
	ev := Event{kind: EventToolResult, toolID: ct.ID}
	if ct.ToolResult != nil {
		var b strings.Builder
		for _, item := range ct.ToolResult.Value.Content {
			if item.Text != "" {
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(item.Text)
			}
		}
		ev.text = b.String()
		ev.isToolError = ct.ToolResult.Value.IsError
	}
	return ev
}
