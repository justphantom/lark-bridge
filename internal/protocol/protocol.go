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
//
// Frontend (feishu-front) constructs this only from a chat message via
// internal/feishufront/dispatcher.go and MUST set just ChatID / Text / Skill
// / CardMessageID. The override fields below — Directory / ModelSpec / Agent /
// Permission / Effort / SettingsFile — are reserved for trusted sources only
// (config, slash-command handlers, in-backend binding mutation). They MUST
// NOT be set by the frontend pipeline: validateSessionDirPath only checks
// IsAbs, so accepting a frontend-supplied Directory would expose an arbitrary
// CWD (claude/opencode running tools in /etc). Backends enforce this with
// HasFrontendOverride at their handlePromptEvent entry.
type PromptPayload struct {
	ChatID    string `json:"chatID"`
	SessionID string `json:"sessionID,omitempty"`
	Directory string `json:"directory,omitempty"`
	Text      string `json:"text"`            // @-stripped
	Skill     bool   `json:"skill,omitempty"` // true: bypass backend slash-command dispatch
	// CardMessageID is the frontend's progress-card message_id for this
	// prompt. Informational, NOT an override (HasFrontendOverride ignores
	// it): a backend whose job outlives the frontend process
	// (deploy-monitor's `make deploy` restarts feishu-front, wiping the
	// in-memory turn map) echoes it back as NoticePayload.UpdateMessageID
	// so the terminal notice patches the original progress card by raw
	// message_id — which survives the restart — instead of falling back to
	// a fresh standalone card.
	CardMessageID string `json:"cardMessageID,omitempty"`
	ModelSpec     string `json:"modelSpec,omitempty"` // user model alias (e.g. "sonnet")
	Agent         string `json:"agent,omitempty"`     // opencode
	Permission    string `json:"permission,omitempty"`
	Effort        string `json:"effort,omitempty"`
	SettingsFile  string `json:"settingsFile,omitempty"`
}

// HasFrontendOverride returns the name of the first override field present
// on this PromptPayload, or "" if none. It is the runtime guard backing the
// "MUST NOT be set by frontend" contract above: a backend's handlePromptEvent
// calls this at entry and rejects any non-empty result with a Notice, so a
// future contributor wiring a quick-task path that short-circuits /cd by
// filling Directory directly surfaces as an explicit protocol violation
// rather than a silent attack surface.
func (p *PromptPayload) HasFrontendOverride() string {
	switch {
	case p.Directory != "":
		return "directory"
	case p.ModelSpec != "":
		return "modelSpec"
	case p.Agent != "":
		return "agent"
	case p.Permission != "":
		return "permission"
	case p.Effort != "":
		return "effort"
	case p.SettingsFile != "":
		return "settingsFile"
	}
	return ""
}

// AnswerPayload carries a user answer to a backend interaction request
// (permission / question). Answer is keyed by RequestID, not PromptID.
// MessageID is the Feishu message ID of the card the user submitted, so the
// backend can update that same card instead of posting a separate notice.
type AnswerPayload struct {
	ChatID    string   `json:"chatID"`
	RequestID string   `json:"requestID"`
	MessageID string   `json:"messageID,omitempty"`
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
