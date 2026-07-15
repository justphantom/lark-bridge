package claude

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// parseContentBlocks extracts the content[] blocks from an assistant or
// user message envelope. Assistant messages yield text/tool_use blocks;
// user messages (tool_result round-trips) yield tool_result blocks.
func parseContentBlocks(lineType, sessionID string, msgRaw json.RawMessage, rawLine string) ([]Event, error) {
	if len(msgRaw) == 0 {
		return nil, nil
	}
	var msg struct {
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgRaw, &msg); err != nil {
		return nil, fmt.Errorf("parse message content: %w", err)
	}

	var out []Event
	for _, b := range msg.Content {
		ev := Event{sessionID: sessionID, raw: rawLine}
		switch b.Type {
		case "text":
			ev.kind = EventText
			ev.text = b.Text
		case "tool_use":
			ev.kind = EventToolUse
			ev.toolID = b.ID
			ev.toolName = b.Name
			ev.toolInput = stringifyJSON(b.Input)
		case "tool_result":
			ev.kind = EventToolResult
			ev.toolID = b.ToolUseID
			ev.text = stringifyContent(b.Content)
			ev.isToolError = b.IsError
		case "thinking":
			ev.kind = EventThinking
			ev.text = b.Text
		default:
			// Unknown block type: keep the raw line for debug, tag with
			// the block type so the bridge can ignore it cheaply.
			ev.kind = lineType + ":" + b.Type
		}
		out = append(out, ev)
	}
	return out, nil
}

// stringifyContent normalises a tool_result "content" field, which the
// CLI emits as either a plain string or an array of content blocks
// (e.g. [{"type":"text","text":"..."}]). Returns "" for nil/empty.
func stringifyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try string first.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// Try an array of text blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Type == "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	// Fallback: raw bytes.
	return strings.TrimSpace(string(raw))
}

// stringifyJSON returns a compacted JSON string for a raw input payload,
// or "" when empty. Used for tool_use input so the bridge can render it
// without re-marshalling. json.Compact preserves the payload verbatim
// (key order, integer precision) where an unmarshal+marshal round trip
// would drop large ints to float64 and reorder keys.
func stringifyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}
