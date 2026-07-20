// Package opencodeserve wraps a running `opencode serve` HTTP server as the
// bridge's agent backend.
//
// The bridge subscribes to the server's global event stream (GET /event SSE),
// drives one session per chat via POST /session/{id}/message?async=true, and
// reduces the resulting event stream into the same shape the CLI-mode bridge
// consumes. A Run returns a channel of parsed Events terminated by a result
// or error event.
package opencodeserve

// EventType constants mirror the opencode CLI bridge's event discriminator so
// the serve bridge's stream loop can switch on the same vocabulary.
const (
	// EventPrompt: synthesised by Run as the FIRST event on the caller's
	// channel, carrying the session id and the client-generated messageID
	// that will tag every part-level event of this turn. Lets callers
	// correlate the SSE stream with the prompt_async POST (whose 204
	// response carries no body). Session-level events (EventSession,
	// idle-synthesised EventResult) keep messageID empty.
	EventPrompt = "prompt"
	// EventSession: session.created arrived, carrying the session id and the
	// model the server bound for this turn.
	EventSession = "session"
	// EventText: an assistant text delta (a chunk of the reply).
	EventText = "text"
	// EventThinking: an assistant reasoning delta.
	EventThinking = "thinking"
	// EventToolUse: a tool invocation start (name + JSON input).
	EventToolUse = "tool_use"
	// EventToolResult: a tool completion (output of a tool).
	EventToolResult = "tool_result"
	// EventStepStart: a new agent step (tool-call round) is beginning.
	EventStepStart = "step_start"
	// EventStepFinish: a non-terminal step completed (reason != "stop").
	// Carries that step's token accounting so the bridge can accumulate the
	// full turn total (a step_finish with reason "stop" becomes EventResult
	// instead; only its tokens would be captured without this event).
	EventStepFinish = "step_finish"
	// EventResult: terminal event with the final answer and run metadata.
	// Synthesised by the client on session.idle.
	EventResult = "result"
	// EventError: synthesised by the client on transport failure, abort, or
	// context cancellation. Terminal like EventResult.
	EventError = "error"
)

// Event is a parsed opencode serve event, flattened for easy consumption. One
// SSE frame yields at most one Event (a tool state transition splits into
// EventToolUse then EventToolResult across frames); a terminal Event
// (EventResult or EventError) is always emitted last.
//
// All fields are unexported on purpose: Event is consumed across package
// boundaries (the serve bridge), and exposing only Get* accessors keeps a
// single read surface so the struct can evolve without breaking callers.
type Event struct {
	kind      string // one of the Event* constants; named "kind" not "type" (reserved word)
	sessionID string
	// messageID tags part-level events (EventText/EventThinking/
	// EventToolUse/EventToolResult/EventStepStart/EventStepFinish) with the
	// assistant message id they belong to. Empty on session-level events
	// (EventPrompt/EventSession/idle-synthesised EventResult).
	messageID string
	text      string

	toolName  string
	toolInput string

	// isToolError flags a tool result with status error/failed.
	isToolError bool

	result  string
	isError bool

	// Token counts from step_finish. input/output are the non-cache
	// breakdown; cacheRead/cacheWrite carry the prompt-cache hits and writes
	// so usage accounting can reconstruct the full picture.
	inputTokens  int
	outputTokens int
	cacheRead    int
	cacheWrite   int

	// cost is the USD cost of the step (0 for cached/free models).
	cost float64
}

// GetType returns the event discriminator (one of the Event* constants).
func (e Event) GetType() string { return e.kind }

// GetSessionID returns the session id from session.created / equivalent events.
func (e Event) GetSessionID() string { return e.sessionID }

// GetMessageID returns the assistant message id tagged on part-level events.
// Empty for session-level events (EventPrompt carries the prompt's user
// messageID; session.created/idle carry none).
func (e Event) GetMessageID() string { return e.messageID }

// GetText returns the assistant text/thinking chunk or tool output.
func (e Event) GetText() string { return e.text }

// GetToolName returns the tool name for tool_use/tool_result events.
func (e Event) GetToolName() string { return e.toolName }

// GetToolInput returns the JSON input summary for a tool_use event.
func (e Event) GetToolInput() string { return e.toolInput }

// GetIsToolError reports whether a tool_result indicates a failed tool call.
func (e Event) GetIsToolError() bool { return e.isToolError }

// GetResult returns the final answer on a result event.
func (e Event) GetResult() string { return e.result }

// GetIsError reports whether this is a terminal error event.
func (e Event) GetIsError() bool { return e.isError }

// GetInputTokens returns the non-cache input token count from the terminal
// step_finish.
func (e Event) GetInputTokens() int { return e.inputTokens }

// GetOutputTokens returns the non-cache output token count from the terminal
// step_finish.
func (e Event) GetOutputTokens() int { return e.outputTokens }

// GetCacheRead returns the cache-read token count from a step_finish.
func (e Event) GetCacheRead() int { return e.cacheRead }

// GetCacheWrite returns the cache-write token count from a step_finish.
func (e Event) GetCacheWrite() int { return e.cacheWrite }

// GetCost returns the USD cost reported by opencode for this step.
func (e Event) GetCost() float64 { return e.cost }
