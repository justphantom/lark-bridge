//go:build linux || darwin

package claude

import (
	"encoding/json"
	"fmt"

	"github.com/justphantom/lark-bridge/internal/strutil"
)

// parseContentBlocks extracts the content[] blocks from an assistant or
// user message envelope. Assistant messages yield text/tool_use blocks;
// user messages (tool_result round-trips) yield tool_result blocks.
func parseContentBlocks(lineType, sessionID string, msgRaw json.RawMessage, rawLine string) ([]Event, error) {
	if len(msgRaw) == 0 {
		return nil, nil
	}
	var msg struct {
		ID      string `json:"id"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
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
		ev := Event{SessionID: sessionID, MessageID: msg.ID, Raw: rawLine}
		switch b.Type {
		case "text":
			ev.Type = EventText
			ev.Text = b.Text
		// server_tool_use is a server-side tool invocation (e.g. webReader)
		// carrying the same id/name/input shape as tool_use. Mapping it to
		// EventToolUse lets the bridge record the id→name pair so the
		// matching tool_result (echoed in an assistant message, not user)
		// renders with the tool name instead of an empty row.
		case "tool_use", "server_tool_use":
			ev.Type = EventToolUse
			ev.ToolID = b.ID
			ev.ToolName = b.Name
			ev.ToolInput = strutil.StringifyJSON(b.Input)
		case "tool_result":
			ev.Type = EventToolResult
			ev.ToolID = b.ToolUseID
			ev.Text = strutil.StringifyContent(b.Content)
			ev.IsToolError = b.IsError
		case "thinking":
			ev.Type = EventThinking
			// Real CLI blocks carry the reasoning in "thinking"; "text"
			// is absent (verified against CLI 2.1.206 capture).
			ev.Text = b.Thinking
		default:
			// Unknown block type: keep the raw line for debug, tag with
			// the block type so the caller can ignore it cheaply.
			ev.Type = lineType + ":" + b.Type
		}
		out = append(out, ev)
	}
	return out, nil
}
