// Package claudebridge glues Feishu events to the Claude Code agent backend.
// One Handler per process owns the router (chatID → Claude session binding)
// and the Claude CLI client. This bridge is stream-driven: one
// `claude -p --output-format stream-json` subprocess per turn, whose stream
// IS the response.
package claudebridge

// promptResult is the value a stream loop delivers once a Claude turn
// finishes (success, error, or cancellation). It is the bridge-internal
// analogue of the opencode bridge's promptResult, trimmed to what a
// stream-json run actually yields.
type promptResult struct {
	reply string // final assistant text (thinking blocks stripped)
	err   error  // non-nil on failure / cancellation
	model string // actual model reported by the CLI on system/init,
	// falling back to the user-pinned modelSpec
	sessionID     string  // session id captured from system/init (may be "")
	durationMs    int64   // from the result event's duration_ms
	contextTokens int     // input+output (actual context window usage)
	costUSD       float64 // from the result event's total_cost_usd
	steps         int     // num_turns from the result event
	isCancelled   bool    // true if the turn was cancelled via /session-abort

	// Token breakdown from the result event, fed to the usage store.
	// contextTokens above mirrors input+output for the result card; these
	// fields add the cache dimensions so a session's full cost is recorded.
	inputTokens   int
	outputTokens  int
	cacheRead     int
	cacheCreation int
}
