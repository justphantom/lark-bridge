package opencodeservebridge

// promptResult is the value a stream loop delivers once an opencode turn
// finishes (success, error, or cancellation). It is the bridge-internal
// reduction of a serve event stream.
type promptResult struct {
	reply string // final assistant text
	err   error  // non-nil on failure / cancellation
	model string // actual model reported by the CLI, falling back to the
	// user-pinned modelSpec
	sessionID     string  // session id captured from the session event (may be "")
	durationMs    int64   // from first step_start to terminal step_finish
	contextTokens int     // input+output (non-cache), aligned with claude
	costUSD       float64 // from step_finish's cost field
	steps         int     // number of step_start events (= agent rounds)
	isCancelled   bool    // true if the turn was cancelled via /session-abort

	// Token breakdown accumulated across every step_finish (both tool-calls
	// steps and the terminal stop step). contextTokens above stays input+
	// output for the result card; these fields let the usage store record the
	// full per-session total including cache, which dominates a resumed turn.
	inputTokens  int
	outputTokens int
	cacheRead    int
	cacheWrite   int
}
