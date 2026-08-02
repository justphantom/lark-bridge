// Package eventmetrics provides lightweight process-level counters for
// event-stream parsing observability. No Prometheus dependency; values are
// exposed via the package-level API and may be exported by backendrpc's
// MetricsReport in the future.
//
// All counters are monotonically-increasing int64 values, safe for concurrent
// use. The package defines pre-named counters for known observability points;
// UnknownEvent creates per-(backend,event_type) counters lazily.
package eventmetrics

import (
	"sync/atomic"
)

// Counter is a monotonically-increasing int64 counter, safe for concurrent use.
type Counter struct {
	v atomic.Int64
}

// Inc adds 1 to the counter.
func (c *Counter) Inc() { c.v.Add(1) }

// Add adds n to the counter.
func (c *Counter) Add(n int64) { c.v.Add(n) }

// Value returns the current count.
func (c *Counter) Value() int64 { return c.v.Load() }

// Reset sets the counter to zero. Intended for tests only.
func (c *Counter) Reset() { c.v.Store(0) }

// ---- Pre-defined counters (F2/F4/F8) ----

// ClaudeResultLenientHit counts how many times parseResultLenient successfully
// recovered a result event after strict json.Unmarshal failed. A hit means
// the Claude CLI's result-line schema has drifted (e.g. a numeric field type
// changed).
var ClaudeResultLenientHit Counter

// ClaudeResultParseFail counts how many times even the lenient fallback
// failed to parse a result-type line. A non-zero value signals a more severe
// schema break.
var ClaudeResultParseFail Counter

// ---- MiniAgent per-turn counters ----

// MiniAgentTurnCount counts how many turns the miniagent backend completed.
var MiniAgentTurnCount Counter

// MiniAgentTurnDurationMs tracks per-turn wall-clock duration in milliseconds.
// Reset is a no-op: this is an append-only counter (cumulative ms).
var MiniAgentTurnDurationMs Counter

// MiniAgentTurnInputTokens and MiniAgentTurnOutputTokens track token usage.
var MiniAgentTurnInputTokens Counter
var MiniAgentTurnOutputTokens Counter

// MiniAgentTurnIncomplete counts turns that ended with FinishMaxIterations
// (hit the LLM-call cap) rather than a clean stop.
var MiniAgentTurnIncomplete Counter

// OMPTextEndFallback counts how many times the bridge used text_end content
// as fallback because no text_delta was received in an assistant round. A
// hit means the OMP CLI omitted text_delta for a message.
var OMPTextEndFallback Counter

// OMPAutoRetryLimit counts how many times the bridge terminated a turn
// because the auto_retry attempt count exceeded the configured limit.
var OMPAutoRetryLimit Counter

// TerminalEmitRetries counts extra emit attempts for terminal controls
// (TypeResult/TypeError/terminal TypeNotice) beyond the first. A non-zero value
// means the frontend's first accept was not confirmed and the backend retried;
// sustained growth signals IPC instability (SSE blips, frontend congestion).
var TerminalEmitRetries Counter

// TerminalEmitLost counts terminal controls whose delivery was never
// confirmed (all retries exhausted / no ACK within the wait budget). Each
// increment is a turn whose final reply card the user never saw — the
// highest-severity observability signal in this package. Backends emit a
// fallback notice when this fires so the turn is not silently stranded.
var TerminalEmitLost Counter

// ---- UnknownEvent dimension ----

// unknownStore holds per-(backend,event_type) counters, created lazily.
var globalUnknownStore = newUnknownStore()

// UnknownEvent returns the Counter for an unrecognised event type in a given
// backend. Counters are created on first access and never removed.
func UnknownEvent(backend, eventType string) *Counter {
	key := backend + "\x00" + eventType
	return globalUnknownStore.get(key)
}

// ---- LineTruncated dimension (F1) ----

// truncatedStore holds per-backend counters for oversized stream lines that
// linereader truncated (the turn continues; the line is parsed best-effort).
var globalTruncatedStore = newUnknownStore()

// LineTruncated returns the Counter for oversized-line truncations in a
// given backend ("claude"/"opencode"/"omp"/"miniagent"). A non-zero value
// means a single stream line exceeded the backend's maxLineLen.
func LineTruncated(backend string) *Counter {
	return globalTruncatedStore.get(backend)
}

// ResetAll resets every pre-defined counter. Intended for tests.
func ResetAll() {
	ClaudeResultLenientHit.Reset()
	ClaudeResultParseFail.Reset()
	OMPTextEndFallback.Reset()
	OMPAutoRetryLimit.Reset()
	TerminalEmitRetries.Reset()
	TerminalEmitLost.Reset()
	globalUnknownStore.reset()
	globalTruncatedStore.reset()
}
