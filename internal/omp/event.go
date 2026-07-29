//go:build linux || darwin

package omp

// Event type constants — the discriminator carried by Event.Type. These map
// the omp --mode json NDJSON line "type" (and, for message_update, the inner
// assistantMessageEvent.type) onto a single value the bridge can switch on.
// See §6.1 / §6.3 for the parser分流 rules.
const (
	// EventSession: the first `session` header line. Carries the session id
	// (id field), cwd, and title. Only the id is consumed (back-filled onto
	// the binding so the next turn --resume continues the session).
	EventSession = "session"
	// EventAgentStart: a new agent round begins (one per turn, plus one per
	// auto_retry). The bridge bumps stepCount. Corresponds to the outer
	// `agent_start` line. NOTE: agent_start fires ONCE per turn even on multi-
	// round (tool-call) turns — the per-round boundary is EventTurnStart, not
	// this event (verified against agnes-2.0-flash, §7.3 Milestone 3).
	EventAgentStart = "agent_start"
	// EventTurnStart: a new assistant turn begins (`turn_start`). This is the
	// real per-round boundary: each tool-call round opens with a turn_start
	// followed by a fresh assistant message. The bridge resets the assistant
	// text accumulator here so the previous round's preamble (notably the
	// inline-thinking text agnes/glm models emit before `</think>` ahead of a
	// tool call) is discarded and only the final round's text becomes the
	// reply. Verified empirically: without this reset the round-1 thinking
	// preamble leaks into the reply (and StripThinking cannot remove it
	// because OMP emits the closing `</think>` without a matching open tag).
	EventTurnStart = "turn_start"
	// EventAgentEnd: the terminal event for a turn (`agent_end`,
	// isTerminal:true). Carries no telemetry in observed versions (§A.1),
	// so usage comes from the accumulated message_end events instead.
	EventAgentEnd = "agent_end"
	// EventMessageUpdate: an assistant text delta. Emitted ONLY for
	// assistantMessageEvent.type text_delta (the `delta` field). text_end is
	// intentionally dropped: it carries the whole block in `content`, but the
	// text_delta events already streamed the full text, so emitting it here
	// doubled the reply (verified against agnes-2.0-flash: a single "pong"
	// reply produced "pongpong"). toolcall_* deltas are dropped here
	// (tool_execution_* events are the authoritative source); done/error are
	// dropped (terminal is decided by message_end/agent_end).
	EventMessageUpdate = "message_update"
	// EventMessageEnd: a message completed. Usage (input/output/cacheRead/
	// cacheWrite/cost) is carried ONLY on role=assistant message_end events
	// (role=toolResult message_end has none); the bridge accumulates the
	// assistant ones. message_start's usage is always zero (§A.6) and
	// ignored.
	EventMessageEnd = "message_end"
	// EventThinking: an assistant thinking block. Emitted ONLY for
	// assistantMessageEvent.type thinking_end (the full block in `content`).
	// thinking_delta is intentionally dropped: the bridge emits TypeThinking
	// with Replace=true, so each partial delta would clobber the zone into an
	// unreadable wall; only the definitive thinking_end block is emitted so
	// the final trace shows cleanly (claude/opencode parity).
	EventThinking = "thinking_delta"
	// EventToolStart: a tool invocation begins (`tool_execution_start` with
	// toolCallId/toolName/args/intent). The bridge emits TypeToolUse.
	EventToolStart = "tool_execution_start"
	// EventToolUpdate: intermediate tool output (`tool_execution_update`).
	// Currently ignored by the bridge (no card zone for partial tool
	// output); kept for forward-compat.
	EventToolUpdate = "tool_execution_update"
	// EventToolEnd: a tool invocation completes (`tool_execution_end` with
	// result/isError). The bridge emits TypeToolResult.
	EventToolEnd = "tool_execution_end"
	// EventNotice: a runtime notice (`notice`). The bridge emits TypeNotice.
	EventNotice = "notice"
	// EventAutoRetry: omp began an automatic retry (`auto_retry_start` with
	// attempt). The bridge emits TypeProgress "自动重试 #N" so the card is
	// not silent during the retry; the turn continues.
	EventAutoRetry = "auto_retry_start"
	// EventError: synthesized by the client on subprocess failure/parse
	// error/cancellation, OR mapped from an assistant message_end whose
	// stopReason is "error" (§10.10). Terminal like EventAgentEnd.
	EventError = "error"
)

// Event is a parsed omp NDJSON event, flattened for easy consumption.
//
// Fields are exported (claude-style) because the bridge reads them directly
// by name in its switch (§7.3): ev.Type, ev.SessionID, ev.Text, ev.Role,
// ev.Attempt, ev.InputTokens, etc. One input line yields exactly one Event
// (the parser routes message_update by inner type into different Event.Type
// values; §6.3).
type Event struct {
	// Type is one of the Event* constants above.
	Type string
	// SessionID is the session id from the `session` header (EventSession).
	SessionID string
	// Text carries:
	//   - EventMessageUpdate: the assistant text delta (delta/content)
	//   - EventThinking: the thinking delta (delta/content)
	//   - EventError: the error message
	Text string
	// ToolName is the tool name for EventToolStart/EventToolEnd.
	ToolName string
	// ToolInput is the tool input summary for EventToolStart (intent if
	// present, else the args JSON). The bridge still routes it through
	// bridgebase.SummarizeToolInput for the card row.
	ToolInput string
	// ToolOutput is the tool result text for EventToolEnd (extracted from
	// result.content[].text).
	ToolOutput string
	// IsToolError flags a tool result with isError=true (EventToolEnd).
	IsToolError bool
	// Role comes from message_end.message.role: the parser extracts usage
	// only for role=assistant (role=toolResult carries no usage). The bridge
	// EventMessageEnd case reads this to decide whether to accumulate.
	Role string
	// Attempt is the retry number from auto_retry_start.attempt, used for
	// the EventAutoRetry progress description ("自动重试 #N").
	Attempt int

	// —— Usage (only on role=assistant message_end; agent_end has none) ——
	InputTokens  int
	OutputTokens int
	CacheRead    int
	CacheWrite   int
	CostUSD      float64

	// IsError flags an EventError (terminal).
	IsError      bool
	ErrorMessage string

	// Raw is retained for debug logging and forward-compat parsing.
	Raw string
}
