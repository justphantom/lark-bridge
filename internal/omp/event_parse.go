//go:build linux || darwin

package omp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ndjsonLine is the flexible envelope decoded from every omp stdout line.
// Only the fields the parser actually consumes are declared; everything else
// is dropped (the schema is large and version-unstable, §A.2).
type ndjsonLine struct {
	Type string `json:"type"`
	// Session header fields (EventSession).
	ID    string `json:"id"`
	Cwd   string `json:"cwd"`
	Title string `json:"title"`
	// Message envelope (message_start/message_end). Kept as RawMessage so
	// the role/usage/stopReason/errorMessage sub-fields decode lazily.
	Message json.RawMessage `json:"message"`
	// Inner assistantMessageEvent (message_update only): carries type +
	// delta/content for text_delta/thinking_delta/etc.
	AssistantMessageEvent json.RawMessage `json:"assistantMessageEvent"`
	// Tool execution fields (tool_execution_*).
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Args       json.RawMessage `json:"args"`
	Intent     string          `json:"intent"`
	Result     json.RawMessage `json:"result"`
	IsError    bool            `json:"isError"`
	// auto_retry_start.
	Attempt int `json:"attempt"`
}

// messageShape is the subset of message_end.message the parser reads.
type messageShape struct {
	Role         string     `json:"role"`
	StopReason   string     `json:"stopReason"`
	ErrorMessage string     `json:"errorMessage"`
	Usage        usageShape `json:"usage"`
}

// usageShape is omp's camelCase usage block. All of input/output/cacheRead/
// cacheWrite/totalTokens plus the nested cost.{input,output,cacheRead,
// cacheWrite,total}. Only the cost.total aggregate is consumed (§6.3).
type usageShape struct {
	Input       int        `json:"input"`
	Output      int        `json:"output"`
	CacheRead   int        `json:"cacheRead"`
	CacheWrite  int        `json:"cacheWrite"`
	TotalTokens int        `json:"totalTokens"`
	Cost        costShape  `json:"cost"`
}

type costShape struct {
	Total float64 `json:"total"`
}

// assistantEventShape is the inner assistantMessageEvent of a message_update
// line. type discriminates the routing (§6.3); delta/content carry the text
// or thinking chunk.
type assistantEventShape struct {
	Type    string `json:"type"`
	Delta   string `json:"delta"`
	Content string `json:"content"`
}

// ParseEvent decodes one NDJSON line into exactly one Event (or zero for
// lines the bridge ignores: message_start, turn_start/turn_end, and the
// toolcall_*/done/error message_update inner types). Exported so tests can
// build Events from real CLI output lines.
func ParseEvent(line string) (Event, bool, error) {
	return parseEvent(line)
}

// parseEvent decodes one NDJSON line. Returns (ev, ok, err): ok=false means
// the line was valid but intentionally ignored (caller treats as no event);
// a non-nil err means the line was malformed (caller logs + continues).
func parseEvent(line string) (Event, bool, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return Event{}, false, nil
	}

	var head ndjsonLine
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		return Event{}, false, fmt.Errorf("parse json: %w", err)
	}

	base := Event{Raw: line}
	switch head.Type {
	// Session header — first line, carries the session id.
	case "session":
		base.Type = EventSession
		base.SessionID = head.ID
		return base, true, nil

	// Agent round begins. The bridge bumps stepCount + resets text.
	case "agent_start":
		base.Type = EventAgentStart
		return base, true, nil

	// Terminal event. agent_end carries no telemetry in observed versions
	// (§A.1); usage comes from the accumulated message_end events.
	case "agent_end":
		base.Type = EventAgentEnd
		return base, true, nil

	// message_start is ignored: usage is always 0 here (§A.6) and any
	// toolCall in content is redundant with the tool_execution_* events
	// that follow (§A.6 observation, §6.3 routing rule).
	case "message_start":
		return Event{}, false, nil

	// message_end: extract usage (role=assistant only); surface an error
	// stopReason as a terminal EventError (§10.10).
	case "message_end":
		return parseMessageEnd(base, head.Message)

	// message_update: route by inner assistantMessageEvent.type (§6.3).
	case "message_update":
		return parseMessageUpdate(base, head.AssistantMessageEvent)

	// Tool lifecycle — the authoritative source for tool rows.
	case "tool_execution_start":
		base.Type = EventToolStart
		base.ToolName = head.ToolName
		base.ToolInput = summariseToolInput(head.Intent, head.Args)
		return base, true, nil
	case "tool_execution_update":
		base.Type = EventToolUpdate
		base.ToolName = head.ToolName
		return base, true, nil
	case "tool_execution_end":
		base.Type = EventToolEnd
		base.ToolName = head.ToolName
		// Pass the same intent/args-derived summary so the result row's
		// "Input" column matches the start row (the end event carries only
		// result + isError, not args/intent). When absent, the bridge's
		// SummarizeToolInput still produces a readable row from ToolName.
		base.ToolOutput = stringifyContent(head.Result)
		base.IsToolError = head.IsError
		return base, true, nil

	// Runtime notice.
	case "notice":
		base.Type = EventNotice
		return base, true, nil

	// Auto-retry: surface as a progress banner so the card is not silent.
	case "auto_retry_start":
		base.Type = EventAutoRetry
		base.Attempt = head.Attempt
		return base, true, nil

	// turn_start is the per-round boundary: each assistant round (incl. each
	// tool-call round) opens with one. The bridge resets the text accumulator
	// here so the previous round's inline-thinking preamble does not leak
	// into the reply (verified empirically against agnes-2.0-flash).
	//
	// turn_end carries nothing actionable (no usage, no terminal signal), so
	// it stays ignored.
	case "turn_start":
		base.Type = EventTurnStart
		return base, true, nil
	case "turn_end":
		return Event{}, false, nil

	default:
		// Forward-compat: surface unrecognised line types for debugging.
		base.Type = head.Type
		return base, true, nil
	}
}

// parseMessageEnd extracts usage from a role=assistant message_end, or
// synthesises a terminal EventError when stopReason=="error" (§10.10).
func parseMessageEnd(base Event, raw json.RawMessage) (Event, bool, error) {
	if len(raw) == 0 {
		// No message envelope: degrade to a bare EventMessageEnd so the
		// bridge's step accounting still ticks.
		base.Type = EventMessageEnd
		return base, true, nil
	}
	var msg messageShape
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Event{}, false, fmt.Errorf("parse message: %w", err)
	}
	// §10.10: a model error surfaces as assistant message with
	// stopReason="error" + errorMessage; there is no standalone error event.
	// Synthesise EventError so the bridge's terminal path fires.
	if msg.StopReason == "error" {
		m := msg.ErrorMessage
		if strings.TrimSpace(m) == "" {
			m = "OMP 运行出错"
		}
		base.Type = EventError
		base.IsError = true
		base.Text = m
		base.ErrorMessage = m
		return base, true, nil
	}
	base.Type = EventMessageEnd
	base.Role = msg.Role
	// Usage is meaningful only on role=assistant (role=toolResult has none).
	// The bridge's EventMessageEnd case gates on Role == "assistant" too, so
	// filling zeros for toolResult is harmless and keeps the struct shape
	// uniform.
	if msg.Role == "assistant" {
		base.InputTokens = msg.Usage.Input
		base.OutputTokens = msg.Usage.Output
		base.CacheRead = msg.Usage.CacheRead
		base.CacheWrite = msg.Usage.CacheWrite
		base.CostUSD = msg.Usage.Cost.Total
	}
	return base, true, nil
}

// parseMessageUpdate routes a message_update line by its inner
// assistantMessageEvent.type per §6.3's分流 table:
//
//	text_delta               → EventMessageUpdate (delta appended by bridge)
//	text_end                 → IGNORED. Carries the whole block in `content`,
//	                           but the text_delta events already streamed the
//	                           full text; emitting it here doubled the reply
//	                           (verified against agnes-2.0-flash: a single
//	                           "pong" reply produced "pongpong").
//	thinking_delta           → IGNORED. The bridge emits TypeThinking with
//	                           Replace=true, which would clobber the zone with
//	                           each partial; only thinking_end (full block) is
//	                           emitted so the final trace is shown cleanly.
//	thinking_end             → EventThinking (content = full thinking block;
//	                           Replace=true in the bridge shows the whole block)
//	toolcall_* / done / error → ignored (tool_execution_* is authoritative)
func parseMessageUpdate(base Event, raw json.RawMessage) (Event, bool, error) {
	if len(raw) == 0 {
		return Event{}, false, nil
	}
	var inner assistantEventShape
	if err := json.Unmarshal(raw, &inner); err != nil {
		return Event{}, false, fmt.Errorf("parse assistantMessageEvent: %w", err)
	}
	switch inner.Type {
	case "text_delta":
		base.Type = EventMessageUpdate
		base.Text = inner.Delta
		return base, true, nil
	case "text_end":
		// Redundant: the deltas already accumulated the full text. Emitting
		// the whole-block content again would double the reply.
		return Event{}, false, nil
	case "thinking_delta":
		// Drop partials: the bridge uses Replace=true, so each partial would
		// clobber the zone. thinking_end carries the definitive full block.
		return Event{}, false, nil
	case "thinking_end":
		// Full thinking block; the bridge emits TypeThinking with Replace=true.
		base.Type = EventThinking
		base.Text = inner.Content
		return base, true, nil
	default:
		// toolcall_start/delta/end (redundant with tool_execution_*), done,
		// error (terminal decided by message_end/agent_end): drop.
		return Event{}, false, nil
	}
}

// summariseToolInput picks the tool input summary at parse time per §6.4:
// intent (the model's declared call purpose, more readable) wins over the
// raw args JSON. The bridge still routes this through
// bridgebase.SummarizeToolInput, which passes non-JSON text through verbatim
// and extracts the key field from JSON args.
func summariseToolInput(intent string, args json.RawMessage) string {
	if intent != "" {
		return intent
	}
	return stringifyJSON(args)
}

// stringifyContent normalises a tool result field ({content:[{type:"text",
// text:"..."}]}) to its concatenated text. omp's tool_execution_end.result
// has this shape (§A.6).
func stringifyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// result: {content:[{type:"text",text:"..."}]}
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Content) > 0 {
		var b strings.Builder
		for _, blk := range envelope.Content {
			if blk.Type == "text" || blk.Type == "" {
				b.WriteString(blk.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	// Fallback: maybe a bare string or content-block array without envelope.
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
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
	return strings.TrimSpace(string(raw))
}

// stringifyJSON returns a compacted JSON string for a raw input payload.
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
