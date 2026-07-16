package peri

// Event kind constants. peri stream-json emits text / tool_use / tool_result
// lines and has NO terminal result or usage line (verified empirically); the
// client synthesizes EventResult at stdout EOF and EventError on failure.
const (
	// EventText: an assistant text chunk.
	EventText = "text"
	// EventToolUse: a tool invocation start (name + id). peri's "input" field
	// is null in stream-json, so tool input is unavailable.
	EventToolUse = "tool_use"
	// EventToolResult: a tool completion (output of a tool). peri does not flag
	// errors structurally; the parser sniffs the "Tool execution failed:" prefix.
	EventToolResult = "tool_result"
	// EventResult: synthesized at stdout EOF from accumulated text chunks.
	EventResult = "result"
	// EventError: synthesized on subprocess failure or cancellation. Terminal.
	EventError = "error"
)

// Event is a parsed peri stream-json event. Fields mirror opencode.Event so a
// bridge layer can consume them via the same Get* accessor pattern.
type Event struct {
	kind string

	// text is the assistant chunk (text) or tool output (tool_result).
	text string

	toolName string
	toolID   string

	// isToolError is inferred from the "Tool execution failed:" output prefix.
	isToolError bool

	// result is the final accumulated reply on a synthesized EventResult.
	result string

	// isError flags a terminal EventError.
	isError bool
}

// GetType returns the event discriminator (one of the Event* constants).
func (e Event) GetType() string { return e.kind }

// GetText returns the assistant text chunk or tool output.
func (e Event) GetText() string { return e.text }

// GetToolName returns the tool name for tool_use/tool_result events.
func (e Event) GetToolName() string { return e.toolName }

// GetToolID returns the peri tool id, correlating a tool_use with its result.
func (e Event) GetToolID() string { return e.toolID }

// GetIsToolError reports whether a tool_result indicates failure (prefix sniff).
func (e Event) GetIsToolError() bool { return e.isToolError }

// GetResult returns the final answer on a synthesized result event.
func (e Event) GetResult() string { return e.result }

// GetIsError reports whether this is a terminal error event.
func (e Event) GetIsError() bool { return e.isError }
