package peribridge

// promptResult is the value a stream loop delivers once a peri turn finishes
// (success, error, or cancellation). It is the bridge-internal reduction of a
// stream-json run.
//
// Unlike opencode's variant, sessionID is always empty (peri print mode has
// no persistence) and the token/cost fields are always zero (peri stream-json
// emits no usage). They are retained so emitTerminal and recordUsage share a
// shape with the opencode bridge, easing future alignment.
type promptResult struct {
	reply string // final assistant text
	err   error  // non-nil on failure / cancellation
	model string // resolved model name (user-pinned spec, or "peri" fallback)

	// sessionID is always "" in peri (print mode is stateless). Kept for
	// structural parity with emitTerminal's result rendering.
	sessionID string

	durationMs    int64   // from first tool_use to stream end
	contextTokens int     // always 0 (peri emits no usage)
	costUSD       float64 // always 0
	steps         int     // number of tool_use events (= tool rounds)
	isCancelled   bool    // true if cancelled via /session-abort or ctx
}
