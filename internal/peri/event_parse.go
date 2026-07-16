package peri

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolErrPrefix is the marker peri prepends to a failed tool's output. There
// is no structural isError field in stream-json (verified), so prefix sniffing
// is the only signal.
const toolErrPrefix = "Tool execution failed:"

// rawLine is the on-wire shape of one peri stream-json line. Fields beyond
// type/content/id/name/input/output are ignored (forward-compat). "input" is
// json.RawMessage because peri emits null for it in stream-json.
type rawLine struct {
	Type    string          `json:"type"`
	Content string          `json:"content,omitempty"`
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  string          `json:"output,omitempty"`
}

// parseLine decodes one stdout line into zero or more Events. textAcc
// accumulates text chunks so a synthesized EventResult at EOF has the full
// reply. Returns (events, isTerminal, err): a line never yields a terminal
// event in peri (no result line exists), so isTerminal is always false here —
// the terminal is synthesized in pump on EOF. The parameter is kept for
// symmetry with opencode's parser shape and future result-line support.
func parseLine(line []byte, textAcc *strings.Builder) (events []Event, isTerminal bool, err error) {
	if len(strings.TrimSpace(string(line))) == 0 {
		return nil, false, nil
	}
	var raw rawLine
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, false, fmt.Errorf("peri line: %w", err)
	}
	switch raw.Type {
	case "text":
		textAcc.WriteString(raw.Content)
		return []Event{{kind: EventText, text: raw.Content}}, false, nil
	case "tool_use":
		return []Event{{kind: EventToolUse, toolName: raw.Name, toolID: raw.ID}}, false, nil
	case "tool_result":
		ev := Event{
			kind:        EventToolResult,
			toolName:    raw.Name,
			toolID:      raw.ID,
			text:        raw.Output,
			isToolError: strings.HasPrefix(raw.Output, toolErrPrefix),
		}
		return []Event{ev}, false, nil
	default:
		// Forward-compat: unknown line types are ignored at debug level by the
		// caller; parseLine signals "no events" so the loop continues.
		return nil, false, nil
	}
}
