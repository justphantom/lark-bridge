package ompbridge

// promptResult is the value a stream loop delivers once an omp turn finishes
// (success, error, or cancellation). It is the bridge-internal reduction of
// an NDJSON run.
type promptResult struct {
	reply string // final assistant text
	err   error  // non-nil on failure / cancellation
	model string // user-pinned modelSpec (omp's NDJSON stream carries no
	// model name on the events the bridge consumes)
	sessionID     string  // session id captured from the `session` header
	durationMs    int64   // from first agent_start to terminal agent_end
	contextTokens int     // input+output (non-cache), aligned with claude
	costUSD       float64 // accumulated from role=assistant message_end events
	steps         int     // number of agent_start events (= agent rounds)
	isCancelled   bool    // true if the turn was cancelled via /session-abort
	// isIdleTimeout is true when the turn was aborted by the idle watchdog
	// (no stdout event for IdleTimeout). Distinct from isCancelled so
	// emitTerminal can show "响应超时" instead of the generic "已取消".
	isIdleTimeout bool
	// stale flags a turn whose error looks like a --resume against a session
	// the CLI no longer has. runPrompt retries once with an empty SessionID
	// when stale is set (§10.7).
	stale bool

	// Token breakdown accumulated across every role=assistant message_end
	// (§7.3 EventMessageEnd case). agent_end carries no telemetry (§A.1), so
	// message_end is the sole source. contextTokens above stays input+
	// output for the result card; these fields let the usage store record
	// the full per-session total including cache, which dominates a resumed
	// turn.
	inputTokens  int
	outputTokens int
	cacheRead    int
	cacheWrite   int
}
