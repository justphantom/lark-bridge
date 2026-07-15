package claude

// EventType constants for the flat type field carried by Event. These
// collapse the Claude Code stream-json line "type" plus the per-block
// "content[].type" into a single discriminator the bridge can switch on.
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
	// the bridge can surface subagent progress instead of dropping it.
	EventTaskStarted      = "task_started"
	EventTaskProgress     = "task_progress"
	EventTaskNotification = "task_notification"
)

// System subtypes, exposed as constants so the bridge can switch on
// Subtype without sprinkling string literals through the consumer code.
const (
	SubtypeInit = "init"
)

// Event is a parsed Claude Code stream-json event, flattened for easy
// consumption. One input line may yield several Events (an assistant
// message can carry multiple content blocks); a terminal Event
// (EventResult or EventError) is always emitted last.
//
// All fields are unexported on purpose: Event is consumed across package
// boundaries (the bridge), and exposing only Get* accessors keeps a
// single read surface so the struct can evolve without breaking callers
// (and satisfies the bridge's narrow claudeEvent interface). Callers
// within this package read and write the fields directly.
type Event struct {
	kind      string // one of the Event* constants; named "kind" not "type" (reserved word)
	subtype   string
	sessionID string
	model     string
	text      string

	toolID    string
	toolName  string
	toolInput string

	// Subagent task fields, populated only on EventTask* events. taskID is
	// the stable identifier correlating started/progress/notification of the
	// same subagent (unlike taskType/taskDesc which drift across the lifecycle);
	// taskType is the subagent type (e.g. "Explore"); taskKind is the task class
	// from upstream ("local_agent" for true subagents, "local_bash" for shell
	// subprocesses); taskDesc is the live description that changes per progress
	// tick; taskTokens/taskSteps/taskMs are the cumulative usage reported by Claude.
	taskID     string
	taskType   string
	taskKind   string
	taskDesc   string
	taskTokens int
	taskSteps  int
	taskMs     int64

	isToolError bool

	result     string
	costUSD    float64
	durationMs int64
	isError    bool
	numTurns   int

	// Token counts from a result line. input/output are the non-cache
	// breakdown; cacheRead/cacheCreation carry the prompt-cache hits and
	// writes so the usage store can record the full per-session picture.
	// The result card still shows input+output only.
	inputTokens   int
	outputTokens  int
	cacheRead     int
	cacheCreation int

	// raw is retained for debug logging and parsing sub-fields (e.g. subagent
	// events) by the bridge.
	raw string
}

// GetType returns the event discriminator (one of the Event* constants).
func (e Event) GetType() string { return e.kind }

// GetSubtype returns the system-line subtype, e.g. SubtypeInit.
func (e Event) GetSubtype() string { return e.subtype }

// GetSessionID returns the session id carried by system/init or result events.
func (e Event) GetSessionID() string { return e.sessionID }

// GetModel returns the model name reported by the CLI for this turn.
func (e Event) GetModel() string { return e.model }

// GetText returns the assistant text/thinking chunk or tool output.
func (e Event) GetText() string { return e.text }

// GetToolID returns the tool-use id. On EventToolUse it carries b.ID, on the
// matching EventToolResult b.ToolUseID, so tool_use/result pairs correlate.
func (e Event) GetToolID() string { return e.toolID }

// GetToolName returns the tool name for tool_use/tool_result events.
func (e Event) GetToolName() string { return e.toolName }

// GetToolInput returns the JSON input summary for a tool_use event.
func (e Event) GetToolInput() string { return e.toolInput }

// GetIsToolError reports whether a tool_result indicates a failed tool call.
func (e Event) GetIsToolError() bool { return e.isToolError }

// GetTaskID returns the stable subagent task identifier.
func (e Event) GetTaskID() string { return e.taskID }

// GetTaskType returns the subagent type (e.g. "Explore").
func (e Event) GetTaskType() string { return e.taskType }

// GetTaskKind returns the task class ("local_agent", "local_bash", ...).
func (e Event) GetTaskKind() string { return e.taskKind }

// GetTaskDesc returns the live subagent description.
func (e Event) GetTaskDesc() string { return e.taskDesc }

// GetTaskTokens returns the cumulative token usage reported for the subagent.
func (e Event) GetTaskTokens() int { return e.taskTokens }

// GetTaskSteps returns the cumulative step count reported for the subagent.
func (e Event) GetTaskSteps() int { return e.taskSteps }

// GetTaskMs returns the cumulative duration reported for the subagent.
func (e Event) GetTaskMs() int64 { return e.taskMs }

// GetResult returns the final answer on a result event.
func (e Event) GetResult() string { return e.result }

// GetIsError reports whether this is a terminal error event.
func (e Event) GetIsError() bool { return e.isError }

// GetCostUSD returns the run cost in USD from a result event.
func (e Event) GetCostUSD() float64 { return e.costUSD }

// GetDurationMs returns the run duration in milliseconds from a result event.
func (e Event) GetDurationMs() int64 { return e.durationMs }

// GetNumTurns returns the number of agentic turns from a result event.
func (e Event) GetNumTurns() int { return e.numTurns }

// GetInputTokens returns the input token count from a result event.
func (e Event) GetInputTokens() int { return e.inputTokens }

// GetOutputTokens returns the output token count from a result event.
func (e Event) GetOutputTokens() int { return e.outputTokens }

// GetCacheRead returns the cache-read (prompt-cache hit) token count from a
// result event. Exposed so the usage store can accumulate cache savings.
func (e Event) GetCacheRead() int { return e.cacheRead }

// GetCacheCreation returns the cache-creation (prompt-cache write) token
// count from a result event.
func (e Event) GetCacheCreation() int { return e.cacheCreation }
