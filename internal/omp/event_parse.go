//go:build linux || darwin

package omp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/strutil"
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
	// notice event fields. Level/Source decode directly; the message body shares
	// the "message" JSON key with the message_* envelope above (Message), so it
	// is read from that RawMessage in the notice case (a notice's message is a
	// bare JSON string, not the assistant-message object).
	Level  string `json:"level"`
	Source string `json:"source"`
	// thinking_level_changed fields.
	ThinkingLevel string `json:"thinkingLevel"`
	Configured    string `json:"configured"`
	Resolved      string `json:"resolved"`
	// auto_retry_start.
	Attempt int `json:"attempt"`
}

// messageShape is the subset of message_end.message the parser reads.
type messageShape struct {
	Role         string     `json:"role"`
	CustomType   string     `json:"customType"` // role=custom system nudge (e.g. mid-run-todo-nudge)
	StopReason   string     `json:"stopReason"`
	ErrorMessage string     `json:"errorMessage"`
	ErrorStatus  int        `json:"errorStatus"` // HTTP-style status (e.g. 429)
	ErrorID      int        `json:"errorId"`     // provider error id (e.g. 135168)
	Usage        usageShape `json:"usage"`
}

// usageShape is omp's camelCase usage block. All of input/output/cacheRead/
// cacheWrite/totalTokens plus the nested cost.{input,output,cacheRead,
// cacheWrite,total}. Only the cost.total aggregate is consumed (§6.3).
type usageShape struct {
	Input       int       `json:"input"`
	Output      int       `json:"output"`
	CacheRead   int       `json:"cacheRead"`
	CacheWrite  int       `json:"cacheWrite"`
	TotalTokens int       `json:"totalTokens"`
	Cost        costShape `json:"cost"`
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
		// Defensive terminal detection: if a malformed line is recognisably
		// an agent_end event, treat it as terminal rather than dropping it.
		// This covers truncated/invalid-agent_end lines that would otherwise
		// trigger the generic "exited without a terminal event" message.
		if looksLikeAgentEnd(line) {
			return Event{Type: EventAgentEnd, Raw: line}, true, nil
		}
		return Event{}, false, fmt.Errorf("parse json: %w", err)
	}

	base := Event{Raw: line}
	switch head.Type {
	// Session header — first line, carries the session id.
	case "session":
		base.Type = EventSession
		base.SessionID = head.ID
		base.SessionTitle = head.Title
		base.SessionCwd = head.Cwd
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
		base.ToolCallID = head.ToolCallID
		base.ToolInput = summariseToolInput(head.Intent, head.Args)
		// Stash the raw args: the end event carries only result+isError, so the
		// bridge joins start→end by ToolCallID and reads args at end to drive
		// todowrite (full todos) and task (subagent_type) special handling.
		base.ToolArgs = strutil.StringifyJSON(head.Args)
		return base, true, nil
	case "tool_execution_update":
		base.Type = EventToolUpdate
		base.ToolName = head.ToolName
		return base, true, nil
	case "tool_execution_end":
		base.Type = EventToolEnd
		base.ToolName = head.ToolName
		base.ToolCallID = head.ToolCallID
		// The end event carries no args/intent (only result + isError). The
		// bridge joins the matching start by ToolCallID for the Input column
		// and the todowrite/task special paths.
		base.ToolOutput = strutil.StringifyContentEnvelope(head.Result)
		base.IsToolError = head.IsError
		return base, true, nil

	// Runtime notice. Forward the message + level (defaulting to "info") so the
	// bridge can emit a TypeNotice instead of dropping it into unknown.
	case "notice":
		base.Type = EventNotice
		base.NoticeLevel = head.Level
		base.NoticeMessage = noticeMessage(head.Message)
		if base.NoticeLevel == "" {
			base.NoticeLevel = "info"
		}
		return base, true, nil

	// thinking_level_changed: the CLI resolved a thinking effort. Forward the
	// resolved value via Text so the bridge can surface an info notice showing
	// the level the model actually uses (not the configured/auto value).
	case "thinking_level_changed":
		base.Type = EventThinkingLevelChanged
		base.Text = head.Resolved
		return base, true, nil

	// In-run reminders. Forwarded for debug visibility only — the bridge logs
	// them and does NOT emit a control: the todowrite tool remains the
	// authoritative todo-list source rendered via TypeTodo, and ttsr is purely
	// diagnostic. Keeping them as named types (not "unknown") stops the
	// unknown-event metric from ticking on every occurrence.
	case "todo_reminder", "ttsr_triggered":
		base.Type = head.Type
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
	// turn_end carries the round's complete assistant message (message.content);
	// forwarded so the bridge can extract its text as a fallback reply when the
	// streaming path produced none (it carries no usage and no terminal signal).
	case "turn_start":
		base.Type = EventTurnStart
		return base, true, nil
	case "turn_end":
		base.Type = EventTurnEnd
		return base, true, nil

	default:
		// Forward-compat: surface unrecognised line types for debugging.
		base.Type = head.Type
		return base, true, nil
	}
}

// looksLikeAgentEnd is a cheap heuristic for recognising an agent_end line
// that JSON decoding could not parse (truncated or otherwise malformed). It
// checks for the literal `"type":"agent_end"` marker so the bridge still
// reaches a terminal event instead of synthesising a vague error.
func looksLikeAgentEnd(line string) bool {
	return strings.Contains(line, `"type":"agent_end"`)
}

// parseMessageEnd extracts usage from a role=assistant message_end, or
// synthesises a terminal EventError when stopReason indicates failure
// ("error" or other terminal reasons like "aborted"/"stopped").
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
	base.Role = msg.Role
	base.ErrorStatus = msg.ErrorStatus
	base.ErrorID = msg.ErrorID

	// role=custom carries a system-injected nudge (e.g. customType
	// "mid-run-todo-nudge"). It has no usage and is never a model error, so
	// surface it as EventMessageEnd with Role=custom + the customType in Text;
	// the bridge emits an info notice and skips usage accumulation. This is
	// checked before the stopReason switch so a custom nudge carrying an
	// incidental stopReason is never misrouted to the error path.
	if msg.Role == "custom" {
		base.Type = EventMessageEnd
		base.Text = msg.CustomType
		return base, true, nil
	}

	// §10.10: a model error surfaces as assistant message with
	// stopReason="error" + errorMessage; there is no standalone error event.
	// Also treat other terminal stopReasons as errors so the bridge's terminal
	// path fires instead of falling through to the generic "no terminal event"
	// message. ErrorStatus/ErrorID (e.g. HTTP 429 / provider error id) are
	// already on base so the bridge can append them to the error text.
	switch msg.StopReason {
	case "error", "aborted", "stopped", "cancelled":
		m := msg.ErrorMessage
		if strings.TrimSpace(m) == "" {
			m = "OMP 运行出错"
			if msg.StopReason != "error" {
				m = "OMP 运行中断（" + msg.StopReason + "）"
			}
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
//	text_end                 → EventTextEnd (content = full block; bridge uses
//	                           this as fallback when no text_delta was received,
//	                           preventing empty assistant replies)
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
		// Return the full text block as EventTextEnd. The bridge normally
		// ignores this event (deltaSeen=true cases skip it) and only uses
		// it as fallback when no text_delta was received, preventing empty
		// assistant replies without reintroducing the "pongpong" duplication
		// bug (verified against agnes-2.0-flash).
		base.Type = EventTextEnd
		base.Text = inner.Content
		return base, true, nil
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
	return strutil.StringifyJSON(args)
}

// noticeMessage extracts the human-readable message body from a notice
// event's "message" JSON value. The same JSON key is shared with the
// message_start/message_end assistant-message envelope (decoded into
// ndjsonLine.Message as a RawMessage), but for a notice the value is a bare
// JSON string (e.g. "xd://: mounted mcp__codegraph_explore"). This unwraps it;
// any non-string payload (absent, object, malformed) yields "" so the bridge
// still emits a notice with an empty body rather than panicking.
func noticeMessage(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
