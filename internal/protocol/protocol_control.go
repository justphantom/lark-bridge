package protocol

import "time"

// Control is the envelope for one backend→frontend message.
type Control struct {
	Type      string `json:"type"`
	BackendID string `json:"backendID,omitempty"` // backfilled by the frontend POST handler from the URL path; empty when the backend sends
	PromptID  string `json:"promptID,omitempty"`
	ChatID string `json:"chatID,omitempty"` // standalone-card controls (Question/Notice) require it

	SessionInit       *SessionInitPayload       `json:"sessionInit,omitempty"`
	Text              *TextPayload              `json:"text,omitempty"`
	Thinking          *ThinkingPayload          `json:"thinking,omitempty"`
	ToolUse           *ToolUsePayload           `json:"toolUse,omitempty"`
	ToolResult        *ToolResultPayload        `json:"toolResult,omitempty"`
	Result            *ResultPayload            `json:"result,omitempty"`
	Error    *ErrorPayload    `json:"error,omitempty"`
	Progress *ProgressPayload `json:"progress,omitempty"`
	Question *QuestionPayload `json:"question,omitempty"`
	Notice   *NoticePayload   `json:"notice,omitempty"`
}

// Control type values.
const (
	TypeSessionInit       = "session_init"
	TypeText              = "text"
	TypeThinking          = "thinking"
	TypeToolUse           = "tool_use"
	TypeToolResult        = "tool_result"
	TypeResult            = "result"
	TypeError    = "error"
	TypeProgress = "progress"
	TypeQuestion = "question"
	TypeNotice   = "notice"
)

// SessionInitPayload announces the session the backend bound for this prompt.
// Model is the actual running model (resolved by the backend from ModelSpec),
// distinct from Event.ModelSpec.
type SessionInitPayload struct {
	SessionID string `json:"sessionID"`
	Directory string `json:"directory,omitempty"`
	Model     string `json:"model,omitempty"`
	Title     string `json:"title,omitempty"`
}

// TextPayload carries a streaming text delta.
type TextPayload struct {
	Delta string `json:"delta"`
}

// ThinkingPayload carries a streaming thinking delta.
type ThinkingPayload struct {
	Delta string `json:"delta"`
}

// ToolUsePayload carries a tool invocation. Input may be a streamed delta or
// the full input for coarse-grained backends. IsSubagent is true when the
// backend knows this row is a subagent/agent delegation rather than a leaf
// tool (claude task_* events; opencode "task" tool), so the frontend can
// summarize subagent counts without guessing from the name. TaskID carries
// the stable subagent identifier (claude only); when set, the frontend
// correlates started/progress/notification of the same subagent by TaskID
// instead of by name (which drifts) or desc (which changes every tick).
type ToolUsePayload struct {
	Name       string `json:"name"`
	Input      string `json:"input,omitempty"`
	IsSubagent bool   `json:"isSubagent,omitempty"`
	TaskID     string `json:"taskId,omitempty"`
}

// ToolResultPayload carries a tool result. Input carries the human-readable
// summary of the invocation (file path, command, etc.) so a result-only
// backend (opencode emits one completed event per call) can still render the
// "Read: /path" prefix on the tool row. IsSubagent mirrors ToolUsePayload;
// TaskID lets a subagent notification close the exact running row opened by
// the matching task_started, even under concurrency.
type ToolResultPayload struct {
	Name       string `json:"name"`
	Input      string `json:"input,omitempty"`
	Output     string `json:"output,omitempty"`
	IsError    bool   `json:"isError,omitempty"`
	IsSubagent bool   `json:"isSubagent,omitempty"`
	TaskID     string `json:"taskId,omitempty"`
}

// ResultPayload is the terminal reply for a prompt.
type ResultPayload struct {
	Text      string        `json:"text"`
	Model     string        `json:"model,omitempty"`
	Tokens    int           `json:"tokens,omitempty"`
	Duration  time.Duration `json:"duration,omitempty"`
	SessionID string        `json:"sessionID,omitempty"`
	Cost      float64       `json:"cost,omitempty"`
	Steps     int           `json:"steps,omitempty"`
	// TotalTokens is the cumulative input+output across every turn of this
	// session so far (including this one), sourced from the backend's usage
	// store. 0 when no history exists (first turn) or usage tracking is off;
	// the renderer hides the cumulative portion in that case.
	TotalTokens int `json:"totalTokens,omitempty"`
}

// ErrorPayload carries an error control. Recoverable hints the frontend can
// retry later.
type ErrorPayload struct {
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

// ProgressPayload carries a free-form progress description.
type ProgressPayload struct {
	Description string `json:"description,omitempty"`
}

// QuestionPayload asks the frontend to render a question card.
type QuestionPayload struct {
	RequestID string         `json:"requestID"`
	PromptID  string         `json:"promptID"`
	Questions []QuestionItem `json:"questions"`
}

// QuestionItem is one question within a QuestionPayload.
type QuestionItem struct {
	Label    string   `json:"label"`
	Options  []string `json:"options"`
	Multiple bool     `json:"multiple,omitempty"`
	Custom   bool     `json:"custom,omitempty"`
}

// NoticePayload carries a notice control (info/success/warning/error share a template).
// Field/Before/After are optional and only set by setting-change slash commands
// (e.g. /perm, /model): when present the frontend renders a before→after block
// instead of the flat Message body, so a user sees what changed relative to the
// prior value. All three are omitted by default (omitempty) so non-change
// notices render exactly as before.
type NoticePayload struct {
	Level   string `json:"level"` // info/success/warning/error
	Title   string `json:"title"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
	Before  string `json:"before,omitempty"`
	After   string `json:"after,omitempty"`
}
