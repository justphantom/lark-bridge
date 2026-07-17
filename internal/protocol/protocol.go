// Package protocol defines the metadata contract between the frontend and
// the backends in the 1-frontend/N-backend split.
//
// Direction convention:
//   - SSE carries Event (frontend→backend): user-side input and actions
//     (Prompt / Answer / Abort / Ping).
//   - POST /v1/control/{backendID} carries Control (backend→frontend):
//     AI-side output and interaction requests (Text / Result / ToolUse /
//     Question / Notice / ...).
//
// This package is pure struct definitions + Validate helpers. No business
// logic. All errors are standard library fmt.Errorf.
package protocol

// === Event (frontend → backend, over SSE) ===

// Event is the envelope for one frontend→backend message.
type Event struct {
	Type     string `json:"type"`
	PromptID string `json:"promptID"` // Prompt/Abort use it; Answer/Ping may be empty
	ChatID   string `json:"chatID,omitempty"`

	Prompt *PromptPayload `json:"prompt,omitempty"`
	Answer *AnswerPayload `json:"answer,omitempty"`
	Abort  *AbortPayload  `json:"abort,omitempty"`
	Ping   *PingPayload   `json:"ping,omitempty"` // non-business heartbeat placeholder
}

// Event type values.
const (
	TypePrompt = "prompt"
	TypeAnswer = "answer"
	TypeAbort  = "abort"
	TypePing   = "ping" // non-business heartbeat placeholder
)

// PromptPayload carries a user prompt. Text has already been @-stripped.
type PromptPayload struct {
	ChatID       string `json:"chatID"`
	SessionID    string `json:"sessionID,omitempty"`
	Directory    string `json:"directory,omitempty"`
	Text         string `json:"text"`                // @-stripped
	Skill        bool   `json:"skill,omitempty"`     // true: bypass backend slash-command dispatch
	ModelSpec    string `json:"modelSpec,omitempty"` // user model alias (e.g. "sonnet")
	Agent        string `json:"agent,omitempty"`     // opencode
	Permission   string `json:"permission,omitempty"`
	Effort       string `json:"effort,omitempty"`
	SettingsFile string `json:"settingsFile,omitempty"`
}

// AnswerPayload carries a user answer to a backend interaction request
// (permission / question). Answer is keyed by RequestID, not PromptID.
type AnswerPayload struct {
	ChatID    string   `json:"chatID"`
	RequestID string   `json:"requestID"`
	Choice    string   `json:"choice,omitempty"`  // permission
	Choices   []string `json:"choices,omitempty"` // question, multiple
	Custom    string   `json:"custom,omitempty"`  // question custom input
}

// AbortPayload aborts an in-flight prompt for a chat/session.
type AbortPayload struct {
	ChatID    string `json:"chatID"`
	SessionID string `json:"sessionID"`
}

// PingPayload is an empty heartbeat payload.
type PingPayload struct{}
