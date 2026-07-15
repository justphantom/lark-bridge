package opencode

// EventType constants for the discriminator carried by Event. These collapse
// the opencode NDJSON line "type" plus per-part "properties.type" into a
// single value the bridge can switch on.
const (
	// EventSession: a session.created (or equivalent) line. Carries the
	// session id and the model the CLI bound for this turn.
	EventSession = "session"
	// EventText: an assistant text part (a chunk of the reply).
	EventText = "text"
	// EventThinking: an assistant reasoning part.
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
	// EventResult: terminal line with the final answer and run metadata
	// (duration, tokens). Always the last event the CLI emits.
	EventResult = "result"
	// EventError: synthesized by the client on subprocess failure, parse
	// error, or context cancellation. Terminal like EventResult.
	EventError = "error"
)

// Event is a parsed opencode NDJSON event, flattened for easy consumption.
// One input line may yield several Events (a message.part.updated line can
// describe a tool transitioning from running to completed); a terminal
// Event (EventResult or EventError) is always emitted last.
//
// All fields are unexported on purpose: Event is consumed across package
// boundaries (the bridge), and exposing only Get* accessors keeps a single
// read surface so the struct can evolve without breaking callers (and
// satisfies the bridge's narrow opencodeEvent interface). Callers within
// this package read and write the fields directly.
type Event struct {
	kind      string // one of the Event* constants; named "kind" not "type" (reserved word)
	sessionID string
	text      string

	toolName  string
	toolInput string

	// isToolError flags a tool result with status error/failed.
	isToolError bool

	result  string
	isError bool

	// Token counts from step_finish. input/output are the non-cache
	// breakdown; cacheRead/cacheWrite carry the prompt-cache hits and writes
	// so usage accounting can reconstruct the full picture. The result card
	// still shows input+output (claude-comparable); cache fields feed the
	// usage store only.
	inputTokens  int
	outputTokens int
	cacheRead    int
	cacheWrite   int

	// cost is the USD cost of the turn (0 for cached/free models).
	cost float64

	// raw is retained for debug logging and forward-compat parsing.
	raw string
}

// GetType returns the event discriminator (one of the Event* constants).
func (e Event) GetType() string { return e.kind }

// GetSessionID returns the session id from session.created / equivalent events.
func (e Event) GetSessionID() string { return e.sessionID }

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
// step_finish. Exposed so the bridge can build a claude-comparable token
// total (input+output) without the cache-read bulk that dominates the wire
// total.
func (e Event) GetInputTokens() int { return e.inputTokens }

// GetOutputTokens returns the non-cache output token count from the terminal
// step_finish.
func (e Event) GetOutputTokens() int { return e.outputTokens }

// GetCacheRead returns the cache-read (prompt-cache hit) token count from a
// step_finish. Exposed so the usage store can accumulate cache savings.
func (e Event) GetCacheRead() int { return e.cacheRead }

// GetCacheWrite returns the cache-write token count from a step_finish.
func (e Event) GetCacheWrite() int { return e.cacheWrite }

// GetCost returns the USD cost reported by opencode for this turn.
func (e Event) GetCost() float64 { return e.cost }
