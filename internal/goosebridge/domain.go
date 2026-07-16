package goosebridge

// promptResult is the value a stream loop delivers once a goose turn finishes
// (success, error, or cancellation). It is the bridge-internal reduction of a
// stream-json run.
//
// sessionID carries the goose --name anchor (e.g. "feishu:oc_xxx"); it is
// empty only before the first successful turn. inputTokens/outputTokens come
// from the complete event and feed the usage store.
type promptResult struct {
	reply string // final assistant text
	err   error  // non-nil on failure / cancellation
	model string // resolved model name (user-pinned spec, or "goose" fallback)

	// sessionID is the goose --name anchor. Back-filled on first success and
	// passed back as --name on subsequent turns.
	sessionID string

	durationMs   int64 // from first tool_use to stream end
	steps        int   // number of tool_use events (= tool rounds)
	isCancelled  bool  // true if cancelled via /session-abort or ctx
	inputTokens  int   // from the complete event
	outputTokens int   // from the complete event
}
