package goose

// Event kind constants. goose stream-json emits message lines (content type
// thinking/text/toolRequest/toolResponse) and one terminal complete line
// carrying token usage. The client synthesizes EventError on failure/cancel.
//
// Verified empirically against goose 1.43.0: every situation exits 0 (even an
// unknown-provider error), so success is judged by whether a complete event
// arrived — never by the subprocess exit code.
const (
	// EventThinking: an assistant reasoning chunk (message.content[].type ==
	// "thinking"). Forwarded to the frontend as TypeThinking.
	EventThinking = "thinking"
	// EventText: an assistant text chunk (message.content[].type == "text").
	EventText = "text"
	// EventToolUse: a tool invocation start (message.content[].type ==
	// "toolRequest"). goose's toolCall.value.name is the tool name; the id
	// correlates the request with its toolResponse.
	EventToolUse = "tool_use"
	// EventToolResult: a tool completion (message.content[].type ==
	// "toolResponse"). Unlike peri (prefix sniff), goose carries a structural
	// isError flag inside toolResult.value.
	EventToolResult = "tool_result"
	// EventComplete: the terminal success event carrying token usage. Emitted
	// once at the end of a healthy run.
	EventComplete = "complete"
	// EventError: synthesized on subprocess failure, parse failure, or
	// cancellation (no complete event reached stdout). Terminal.
	EventError = "error"
)

// Event is a parsed goose stream-json event. Fields mirror peri.Event so a
// bridge layer can consume them via the same Get* accessor pattern, with two
// additions: thinking text and token usage (both absent from peri).
type Event struct {
	kind string

	// text is the assistant chunk (text/thinking) or tool output (tool_result).
	text string

	toolName string
	toolID   string

	// isToolError is read from toolResult.value.isError (structural, not sniffed).
	isToolError bool

	// inputTokens/outputTokens are populated only on EventComplete (the final
	// usage line). Zero on every other kind.
	inputTokens  int
	outputTokens int

	// isError flags a terminal EventError.
	isError bool
}

// GetType returns the event discriminator (one of the Event* constants).
func (e Event) GetType() string { return e.kind }

// GetText returns the assistant text/thinking chunk or tool output.
func (e Event) GetText() string { return e.text }

// GetToolName returns the tool name for tool_use/tool_result events.
func (e Event) GetToolName() string { return e.toolName }

// GetToolID returns the goose tool id, correlating a toolRequest with its
// toolResponse.
func (e Event) GetToolID() string { return e.toolID }

// GetIsToolError reports whether a tool_result indicates failure (structural
// isError flag from toolResult.value, not a prefix sniff).
func (e Event) GetIsToolError() bool { return e.isToolError }

// GetInputTokens returns the input token count on EventComplete; 0 otherwise.
func (e Event) GetInputTokens() int { return e.inputTokens }

// GetOutputTokens returns the output token count on EventComplete; 0 otherwise.
func (e Event) GetOutputTokens() int { return e.outputTokens }

// GetIsError reports whether this is a terminal error event.
func (e Event) GetIsError() bool { return e.isError }

// NewThinkingEvent builds an EventThinking. Exported for tests that script a
// goose event stream without a real subprocess.
func NewThinkingEvent(text string) Event {
	return Event{kind: EventThinking, text: text}
}

// NewTextEvent builds an EventText.
func NewTextEvent(text string) Event {
	return Event{kind: EventText, text: text}
}

// NewToolUseEvent builds an EventToolUse.
func NewToolUseEvent(name, id string) Event {
	return Event{kind: EventToolUse, toolName: name, toolID: id}
}

// NewToolResultEvent builds an EventToolResult. isErr sets the structural
// isError flag directly so tests do not need to construct the nested value.
func NewToolResultEvent(name, id, output string, isErr bool) Event {
	return Event{kind: EventToolResult, toolName: name, toolID: id, text: output, isToolError: isErr}
}

// NewCompleteEvent builds an EventComplete carrying the turn's token usage.
func NewCompleteEvent(input, output int) Event {
	return Event{kind: EventComplete, inputTokens: input, outputTokens: output}
}

// NewErrorEvent builds a terminal EventError.
func NewErrorEvent(text string) Event {
	return Event{kind: EventError, text: text, isError: true}
}
