package protocol

// StatusSnapshot is the JSON body returned by the frontend's GET /v1/status —
// the single source of truth for "which turns are in flight right now". A
// backend (e.g. deploy-monitor's /running) consumes it to render the live turn
// list instead of guessing from its own partial view. Field tags match the
// frontend's response exactly.
type StatusSnapshot struct {
	InFlight int        `json:"inflight"`
	Backends []string   `json:"backends"`
	Turns    []TurnInfo `json:"turns"`
}

// TurnInfo is one in-flight turn's identity, as exposed by GET /v1/status.
// ElapsedS is wall-clock seconds since the turn started.
type TurnInfo struct {
	PromptID  string `json:"prompt_id"`
	ChatID    string `json:"chat_id"`
	BackendID string `json:"backend_id"`
	ElapsedS  int64  `json:"elapsed_s"`
}
