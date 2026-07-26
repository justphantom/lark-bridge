//go:build linux || darwin

package claude

import "strings"

// EventType constants for the flat Type field carried by Event. These
// collapse the Claude Code stream-json line "type" plus the per-block
// "content[].type" into a single discriminator the caller can switch on.
const (
	// EventSystem: a system line. Subtype discriminates further:
	// "init" (carries the session id), "thinking_tokens".
	EventSystem = "system"
	// EventText: an assistant text content block (a chunk of the reply).
	EventText = "text"
	// EventThinking: an assistant thinking content block (reasoning trace).
	EventThinking = "thinking"
	// EventToolUse: an assistant tool invocation (name + JSON input).
	EventToolUse = "tool_use"
	// EventToolResult: a tool_result block echoed back (output of a tool).
	EventToolResult = "tool_result"
	// EventResult: terminal line (subtype success/error) with the final
	// answer and run metadata (cost, duration). Always the last event.
	EventResult = "result"
	// EventError: synthesized by the client on subprocess failure, parse
	// error, or context cancellation. Terminal like EventResult.
	EventError = "error"

	// Subagent task lifecycle. Claude emits these as system lines with a
	// task_* subtype when a Task/Agent tool spawns a local subagent. They
	// carry the subagent type, a live description, and cumulative usage so
	// the caller can surface subagent progress instead of dropping it.
	EventTaskStarted      = "task_started"
	EventTaskProgress     = "task_progress"
	EventTaskNotification = "task_notification"
)

// System subtypes, exposed as constants so callers can switch on Subtype
// without sprinkling string literals through the consumer code.
const (
	SubtypeInit = "init"
)

// Event is a parsed Claude Code stream-json event, flattened for easy
// consumption. One input line may yield several Events (an assistant
// message can carry multiple content blocks); a terminal Event
// (EventResult or EventError) is always emitted last.
type Event struct {
	Type      string // one of the Event* constants
	Subtype   string
	SessionID string
	Model     string
	Text      string

	ToolID    string
	ToolName  string
	ToolInput string

	// Subagent task fields, populated only on EventTask* events. TaskID is
	// the stable identifier correlating started/progress/notification of the
	// same subagent (unlike TaskType/TaskDesc which drift across the lifecycle);
	// TaskType is the subagent type (e.g. "Explore"); TaskKind is the task class
	// from upstream ("local_agent" for true subagents, "local_bash" for shell
	// subprocesses); TaskDesc is the live description that changes per progress
	// tick; TaskTokens/TaskSteps/TaskMs are the cumulative usage reported by Claude.
	TaskID     string
	TaskType   string
	TaskKind   string
	TaskDesc   string
	TaskTokens int
	TaskSteps  int
	TaskMs     int64

	IsToolError bool

	Result     string
	CostUSD    float64
	DurationMs int64
	IsError    bool
	NumTurns   int

	// StopReason is the model's stop_reason from the result line
	// (e.g. "end_turn"); DurationAPIMs is the API-only wall time
	// (duration_api_ms), complementing DurationMs which includes CLI
	// overhead.
	StopReason    string
	DurationAPIMs int64

	// Token counts from a result line. InputTokens/OutputTokens are the
	// non-cache breakdown; CacheRead/CacheCreation carry the prompt-cache
	// hits and writes so callers can record the full per-session picture.
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheCreation int

	// Raw is retained for debug logging and parsing sub-fields (e.g.
	// subagent events) by the caller.
	Raw string
}

// IsStaleSession reports whether e is the CLI's "session no longer
// exists" terminal error (result line, is_error, "No conversation found
// with session ID: …"). Centralised here so consumers don't hand-roll
// string matching — the CLI may reword the message, and only this
// substring is checked so a fix applies in one place.
func IsStaleSession(e Event) bool {
	return e.Type == EventResult && e.IsError &&
		strings.Contains(e.Result, "No conversation found")
}
