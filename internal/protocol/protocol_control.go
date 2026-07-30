package protocol

import "time"

// Control is the envelope for one backend→frontend message.
type Control struct {
	Type      string `json:"type"`
	BackendID string `json:"backendID,omitempty"` // backfilled by the frontend POST handler from the URL path; empty when the backend sends
	PromptID  string `json:"promptID,omitempty"`
	ChatID    string `json:"chatID,omitempty"` // standalone-card controls (Question/Notice) require it

	SessionInit  *SessionInitPayload  `json:"sessionInit,omitempty"`
	Text         *TextPayload         `json:"text,omitempty"`
	Thinking     *ThinkingPayload     `json:"thinking,omitempty"`
	ToolUse      *ToolUsePayload      `json:"toolUse,omitempty"`
	ToolResult   *ToolResultPayload   `json:"toolResult,omitempty"`
	Result       *ResultPayload       `json:"result,omitempty"`
	Error        *ErrorPayload        `json:"error,omitempty"`
	Progress     *ProgressPayload     `json:"progress,omitempty"`
	Todo         *TodoPayload         `json:"todo,omitempty"`
	Question     *QuestionPayload     `json:"question,omitempty"`
	Permission   *PermissionPayload   `json:"permission,omitempty"`
	Notice       *NoticePayload       `json:"notice,omitempty"`
	File         *FilePayload         `json:"file,omitempty"`
	StatusReport *StatusReportPayload `json:"statusReport,omitempty"`
}

// Control type values.
const (
	TypeSessionInit  = "session_init"
	TypeText         = "text"
	TypeThinking     = "thinking"
	TypeToolUse      = "tool_use"
	TypeToolResult   = "tool_result"
	TypeResult       = "result"
	TypeError        = "error"
	TypeProgress     = "progress"
	TypeTodo         = "todo"
	TypeQuestion     = "question"
	TypePermission   = "permission"
	TypeNotice       = "notice"
	TypeFile         = "file"
	TypeStatusReport = "status_report"
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

// ThinkingPayload carries a streaming thinking delta. Replace=true marks an
// authoritative snapshot (the SDK's HighEventThinkingDone / part-end frame):
// the renderer RESETS its thinking buffer to Delta instead of appending, so a
// dropped/out-of-order delta earlier in the part self-heals. Replace=false
// (default, including the legacy zero value) appends. Empty Delta + Replace
// clears the buffer.
type ThinkingPayload struct {
	Delta   string `json:"delta"`
	Replace bool   `json:"replace,omitempty"`
}

// ToolUsePayload carries a tool invocation. Input may be a streamed delta or
// the full input for coarse-grained backends. IsSubagent is true when the
// backend knows this row is a subagent/agent delegation rather than a leaf
// tool (claude task_* events; opencode "task" tool), so the frontend can
// summarize subagent counts without guessing from the name. TaskID carries
// the stable subagent identifier (claude only); when set, the frontend
// correlates started/progress/notification of the same subagent by TaskID
// instead of by name (which drifts) or desc (which changes every tick).
//
// Subagent, when non-nil, drives the dedicated subagent zone on the progress
// card (claude local_agent task_started/task_progress; opencode has no
// running-phase event so it only fills ToolResultPayload.Subagent). When
// Subagent is nil the renderer falls back to the legacy leaf-tool row — the
// IsSubagent flag still marks it for category counting.
type ToolUsePayload struct {
	Name       string           `json:"name"`
	Input      string           `json:"input,omitempty"`
	IsSubagent bool             `json:"isSubagent,omitempty"`
	TaskID     string           `json:"taskId,omitempty"`
	Subagent   *SubagentSummary `json:"subagent,omitempty"`
}

// ToolResultPayload carries a tool result. Input carries the human-readable
// summary of the invocation (file path, command, etc.) so a result-only
// backend (opencode emits one completed event per call) can still render the
// "Read: /path" prefix on the tool row. IsSubagent mirrors ToolUsePayload;
// TaskID lets a subagent notification close the exact running row opened by
// the matching task_started, even under concurrency.
//
// Subagent, when non-nil, carries the terminal summary (preview, output size,
// final status) that the subagent zone renders on completion. Nil Subagent
// keeps the legacy leaf-tool row.
type ToolResultPayload struct {
	Name       string           `json:"name"`
	Input      string           `json:"input,omitempty"`
	Output     string           `json:"output,omitempty"`
	IsError    bool             `json:"isError,omitempty"`
	IsSubagent bool             `json:"isSubagent,omitempty"`
	TaskID     string           `json:"taskId,omitempty"`
	Subagent   *SubagentSummary `json:"subagent,omitempty"`
}

// SubagentSummary is the structured metadata for a subagent delegation,
// surfaced through ToolUsePayload.Subagent (running/progress) and
// ToolResultPayload.Subagent (terminal). It lifts a subagent out of the
// leaf-tool-row rendering into a dedicated zone on the progress card.
//
// Field sourcing differs per backend: opencode inlines all of this in its single completed task
// event; claude local_agent distributes it across task_started (Type/Title/
// TaskType/ChildSession) + task_progress (Description/ToolUses/DurationMs/
// LastToolName/TotalTokens) + task_notification (Preview/OutputBytes/Status).
// claude local_bash carries no Subagent at all (renderer keeps it as a leaf
// Bash row), so the Subagent == nil fallback path stays exercised.
//
// All optional fields use omitempty so a backend that lacks one (e.g. claude
// never sets Model/Truncated; opencode never sets ToolUses/LastToolName)
// produces a compact JSON and the renderer omits the corresponding line.
type SubagentSummary struct {
	// Status drives the row state. "running" (opened, optional progress
	// updates from claude task_progress), "completed", or "failed".
	// ToolUse always carries running; ToolResult carries completed/failed.
	Status string `json:"status"`

	// TaskType discriminates the delegation kind. claude: "local_agent"
	// (true AI subagent, rendered in the dedicated zone) or "local_bash"
	// (background shell, rendered as leaf Bash row — Subagent stays nil
	// then); opencode: "agent" (its only kind).
	TaskType string `json:"taskType,omitempty"`

	// Type is the subagent_type (claude "general-purpose"; opencode
	// "explore"/"general"/...). Rendered as the zone's leading label.
	Type string `json:"type,omitempty"`

	// Title is the task's short description (task_started.description /
	// state.title / state.input.description). Stable across the lifecycle.
	Title string `json:"title,omitempty"`

	// Description is the live action text (claude task_progress only,
	// e.g. "Reading internal/lark/ws/frame.go"). Updates every progress.
	Description string `json:"description,omitempty"`

	// ChildSession is the stable subagent identifier: claude task_id
	// (resolvable to the subagent's own jsonl via output_file symlink) or
	// opencode metadata.sessionId.
	ChildSession string `json:"childSession,omitempty"`

	// Model is the subagent's model id. opencode metadata.model.modelID;
	// claude local_agent does not carry it (the renderer omits the line).
	Model string `json:"model,omitempty"`

	// DurationMs is the cumulative wall time. claude usage.duration_ms
	// (updated every task_progress); opencode time.end - time.start.
	DurationMs int64 `json:"durationMs,omitempty"`

	// ToolUses is the cumulative tool-call count inside the subagent
	// (claude usage.tool_uses). opencode has no equivalent (single event).
	ToolUses int `json:"toolUses,omitempty"`

	// LastToolName is the most recent tool the subagent ran (claude
	// usage.last_tool_name). Drives the "正在 Read" running hint.
	LastToolName string `json:"lastToolName,omitempty"`

	// TotalTokens is the cumulative token count (claude usage.total_tokens).
	// Often 0 when the provider does not report tokens; the renderer hides
	// the segment when 0 to avoid a misleading "0 tokens" line.
	TotalTokens int `json:"totalTokens,omitempty"`

	// Preview is the first ~200 runes of the terminal output, capped by
	// the backend (claude strips task_notification.summary; opencode
	// strips the <task>...<task_result> XML wrapper). Terminal-only.
	Preview string `json:"preview,omitempty"`

	// OutputBytes is the full output length in bytes, giving a "产出 NKB"
	// hint without inlining the whole output. Terminal-only.
	OutputBytes int `json:"outputBytes,omitempty"`

	// Truncated reports whether the subagent's output was truncated by the
	// upstream (opencode metadata.truncated). claude does not report it.
	Truncated bool `json:"truncated,omitempty"`
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
	// session so far (including this one). Sourced authoritatively from the
	// SDK's SessionTokens (GetSession-backed); falls back to the backend's
	// usage store when the SDK returned 0 (GetSession failed). 0 when no
	// history exists (first turn); the renderer hides the cumulative portion
	// in that case.
	TotalTokens int `json:"totalTokens,omitempty"`
	// TotalCost is the cumulative cost across every turn of this session so
	// far (including this one), sourced authoritatively from the SDK's
	// SessionCost; falls back to the usage store on 0. The renderer shows
	// "本次 / 累计" mirroring TotalTokens when TotalCost exceeds Cost.
	TotalCost float64 `json:"totalCost,omitempty"`
	// ReasoningTokens is this turn's reasoning-token usage (thinking models
	// only). 0 for non-reasoning turns; the renderer hides the line then.
	ReasoningTokens int `json:"reasoningTokens,omitempty"`
	// Incomplete is true when the backend hit its iteration cap without
	// producing a final answer (miniagent: maxIterations=20). Text is empty
	// in that case; without this flag the frontend cannot distinguish a
	// legitimately empty reply from a truncated one. miniagent only.
	Incomplete bool `json:"incomplete,omitempty"`
}

// ErrorPayload carries an error control. Recoverable hints the frontend can
// retry later.
type ErrorPayload struct {
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable,omitempty"`
}

// ProgressPayload carries a free-form progress description and an optional gate
// banner. Description is a transient grey status line (e.g. a picker's
// "loading" notice). Gate surfaces a blocking interactive gate
// (permission/question) on the streaming progress card itself so the user sees
// the agent is waiting without scrolling to the standalone gate card. Both
// render through the same banner slot; a non-empty Gate takes precedence.
type ProgressPayload struct {
	Description string    `json:"description,omitempty"`
	Gate        *GateInfo `json:"gate,omitempty"`
}

// GateInfo is a banner on the progress card marking an interactive gate that
// is blocking the in-flight turn. State drives the icon/verb the renderer
// applies to Summary (waiting=⏸, answered=✓, denied=✗); Kind labels the gate
// type for the verb ("等待授权" vs "等待回答"). Summary is the bare content
// (the action under permission, or the picked choice on answer), NOT a
// pre-formatted line — the renderer owns presentation.
type GateInfo struct {
	State   string `json:"state"`          // waiting|answered|denied
	Kind    string `json:"kind,omitempty"` // permission|question
	Summary string `json:"summary,omitempty"`
}

// TodoPayload carries the session's full todo list. The backend sends the
// complete list on every update (no incremental merge), so the renderer
// overwrites its prior state with each arrival.
type TodoPayload struct {
	Todos []TodoItem `json:"todos"`
}

// TodoItem is one entry of a todo list (content / status / priority), aligned
// with the SDK's Todo. All fields are value types, so a single copy is a deep
// copy.
type TodoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`             // pending|in_progress|completed|cancelled
	Priority string `json:"priority,omitempty"` // high|medium|low
}

// QuestionPayload asks the frontend to render a question card.
type QuestionPayload struct {
	RequestID string         `json:"requestID"`
	PromptID  string         `json:"promptID"`
	Questions []QuestionItem `json:"questions"`
	// TakeOverProgress marks a slash-command picker card (/model, /cd…): the
	// frontend morphs the progress card it opened for the command message into
	// this question card instead of shipping a standalone one, so the whole
	// command→pick→result interaction lives on one card. Mid-turn
	// permission/question cards leave it unset and ship standalone.
	TakeOverProgress bool `json:"takeOverProgress,omitempty"`
	// UpdateMessageID turns a question control into an in-place refresh of an
	// existing picker card: instead of morphing the progress card or shipping a
	// standalone card, the frontend PATCHes THIS message_id with the new
	// options. Used by multi-round pickers (notably /send's directory browser)
	// so descending into a directory updates the same card instead of leaving
	// the prior picker behind and piling up a new card per level. The PATCH is
	// delayed past Feishu's click-handling window by the frontend. Empty on the
	// first round (which morphs the progress card); set to that card's
	// message_id on every subsequent round. The RequestID must still be fresh
	// per round so clicks on the old option set cannot collide with the new one.
	UpdateMessageID string `json:"updateMessageID,omitempty"`
}

// QuestionItem is one question within a QuestionPayload.
type QuestionItem struct {
	Label    string   `json:"label"`
	Options  []string `json:"options"`
	Multiple bool     `json:"multiple,omitempty"`
	Custom   bool     `json:"custom,omitempty"`
}

// PermissionPayload asks the frontend to render a permission card as a row of
// buttons (one per option). Unlike QuestionPayload's dropdown+submit flow, a
// permission card has a small fixed option set, so each option is a direct
// button whose click submits immediately. Value is the machine value returned
// in the answer's Choices[0]; Label is what the user sees on the button.
//
// Structured body: Type/Title/Detail give a typed rendering (badge + headline +
// detail block). A renderer that understands them prefers Title over Message;
// one that does not (older frontends) falls back to Message. Callers should
// keep Message populated for that fallback.
type PermissionPayload struct {
	RequestID string `json:"requestID"`
	PromptID  string `json:"promptID"`
	Message   string `json:"message,omitempty"`
	// Type is the permission category (Bash/Write/Edit/...), rendered as a
	// bold badge above the title.
	Type string `json:"type,omitempty"`
	// Title is the one-line headline (command or target file). When empty the
	// renderer falls back to Message so a caller that only sets Message (e.g.
	// a mode picker) renders exactly as before.
	Title string `json:"title,omitempty"`
	// Detail is the full text (patterns, full command) shown in a code block
	// under the title. Optional; when equal to Title the renderer omits the
	// duplicate block.
	Detail  string             `json:"detail,omitempty"`
	Options []PermissionOption `json:"options"`
	// TakeOverProgress mirrors QuestionPayload: a slash-command picker morphs
	// the progress card; a mid-turn permission gate ships standalone.
	TakeOverProgress bool `json:"takeOverProgress,omitempty"`
}

// PermissionMessage is the structured body carrier AskPermission accepts, so a
// permission card can carry Type/Title/Detail (structured render) without
// forcing every caller to set them. A caller that only needs the legacy flat
// body sets Message alone; the renderer falls back to it when Title is empty.
type PermissionMessage struct {
	Message string
	Type    string
	Title   string
	Detail  string
}

// PermissionOption is one button of a permission card.
type PermissionOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// NoticePayload carries a notice control (info/success/warning/error share a template).
// Field/Before/After are optional and only set by setting-change slash commands
// (e.g. /perm, /model): when present the frontend renders a before→after block
// instead of the flat Message body, so a user sees what changed relative to the
// prior value. All three are omitted by default (omitempty) so non-change
// notices render exactly as before.
// UpdateMessageID, when set, tells the frontend to patch the existing card
// rather than sending a new standalone notice card.
type NoticePayload struct {
	Level           string `json:"level"` // info/success/warning/error
	Title           string `json:"title"`
	Message         string `json:"message,omitempty"`
	Field           string `json:"field,omitempty"`
	Before          string `json:"before,omitempty"`
	After           string `json:"after,omitempty"`
	UpdateMessageID string `json:"updateMessageID,omitempty"`
}

// FilePayload carries a file from a backend to the frontend so the frontend
// can upload it to Feishu and send it to the chat (send-file-design.md §3.1).
// The backend owns reading the file out of the bound working directory; the
// frontend owns the Feishu credentials and network egress, so the backend
// never calls the IM API directly. Content is the raw bytes base64-encoded,
// which keeps the existing JSON-over-POST/SSE protocol binary-free without a
// new streaming channel (the 30 MiB Feishu cap → ~40 MiB base64, within IPC
// budgets for the rare large-file case).
//
// UpdateMessageID, when set, tells the frontend to PATCH the picker card the
// user just clicked (the card whose answer carried this messageID) with the
// send outcome instead of posting a separate result notice.
type FilePayload struct {
	// ChatID is the destination chat. Mirrors Control.ChatID (the validator
	// checks the top-level field); kept on the payload so a self-contained
	// FilePayload is usable without the envelope.
	ChatID string `json:"chatID"`
	// FileName is the display name and the Feishu upload file_name.
	FileName string `json:"fileName"`
	// MIMEType is optional; the frontend infers from the extension when empty.
	MIMEType string `json:"mimeType,omitempty"`
	// Content is the file's raw bytes, base64 (StdEncoding) encoded.
	Content string `json:"content"`
	// UpdateMessageID optionally targets an existing card to PATCH with the
	// outcome; empty → the frontend sends a standalone result notice.
	UpdateMessageID string `json:"updateMessageID,omitempty"`
}

// StatusReportPayload is sent periodically by the status-monitor backend to
// refresh a standing "overview" card on every chat bound to it. The frontend
// keys its messageID bookkeeping on (chatID, Key): present → PATCH in place,
// absent (or PATCH returns isCardGone) → SendCard a new one. ChatID is set by
// the frontend during broadcast (one card per bound chat), NOT by the backend.
//
// Data mirrors GET /v1/status's StatusSnapshot (the frontend is the source of
// truth for in-flight turns), so the backend effectively forwards the snapshot
// plus presentation metadata (Title/IntervalS/GeneratedAt). Turns reuses the
// protocol.TurnInfo type so the wire shape matches /v1/status exactly.
type StatusReportPayload struct {
	Key         string        `json:"key"`                 // stable id (default = backendID); frontend keys messageID on chatID|key
	GeneratedAt int64         `json:"generatedAt"`         // unix seconds, backend's clock at assembly time
	IntervalS   int           `json:"intervalS,omitempty"` // refresh period, display-only
	Title       string        `json:"title,omitempty"`     // card title
	InFlight    int           `json:"inflight"`            // in-flight turn count
	Backends    []string      `json:"backends,omitempty"`  // online backend IDs
	Turns       []TurnInfo    `json:"turns,omitempty"`     // per in-flight turn (reuses protocol.TurnInfo)
	Hosts       []HostStats   `json:"hosts,omitempty"`     // per-host load snapshot
	Services    []ServiceStat `json:"services,omitempty"`  // per-backend process snapshot
}
