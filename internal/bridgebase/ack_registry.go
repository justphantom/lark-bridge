package bridgebase

import (
	"context"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// AckRegistry pairs each terminal Control's PromptID with a one-shot wait the
// EmitTerminal retry loop arms. The frontend's ACK (a protocol.Event{Type:TypeAck})
// resolves the wait via Resolve, signalling the loop that the terminal control
// was received and rendered — so the backend stops retrying instead of
// re-sending duplicates or, worse, giving up silently.
//
// Design notes:
//   - Keyed by PromptID (every terminal control carries one). ACKs carry the
//     same PromptID, so the pairing is exact with no extra correlation id.
//   - One-shot: a wait is consumed exactly once (Resolve closes the channel and
//     deletes the entry). A late/duplicate ACK for an already-resolved or
//     already-expired wait is a no-op (logged at debug).
//   - Bounded: each wait has its own timeout (the EmitTerminal send budget).
//     Wait never blocks past that — if no ACK arrives, the retry loop's own
//     deadline fires and the wait is reaped by the loop calling Forget.
//   - Close drains all pending waits so a Core shutdown does not leave the
//     EmitTerminal goroutines blocked forever.
type AckRegistry struct {
	mu      sync.Mutex
	waiters map[string]chan struct{}
	logger  *log.Logger
}

// NewAckRegistry builds an empty registry. logger is used for debug traces of
// late/duplicate ACKs; nil → no-op.
func NewAckRegistry(logger *log.Logger) *AckRegistry {
	if logger == nil {
		logger = log.Nop()
	}
	return &AckRegistry{
		waiters: make(map[string]chan struct{}),
		logger:  logger,
	}
}

// Arm registers a one-shot wait for promptID and returns the channel that Resolve
// closes on ACK. The caller MUST call Forget(promptID) once it is done with the
// wait (ACK received, or the retry budget exhausted) so the entry is reaped.
// Arm is idempotent for a still-pending promptID: re-arming returns the existing
// channel (the retry loop re-arms between attempts on the same promptID).
func (r *AckRegistry) Arm(promptID string) chan struct{} {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ch, ok := r.waiters[promptID]; ok {
		return ch
	}
	ch := make(chan struct{})
	r.waiters[promptID] = ch
	return ch
}

// Resolve signals the wait for promptID (the frontend confirmed delivery).
// No-op if no wait is pending (a late/duplicate ACK after the loop expired) or
// the wait was already resolved — both are logged at debug for observability.
func (r *AckRegistry) Resolve(promptID string) {
	r.mu.Lock()
	ch, ok := r.waiters[promptID]
	if ok {
		delete(r.waiters, promptID)
	}
	r.mu.Unlock()
	if ok {
		close(ch)
		return
	}
	r.logger.Debug("ack: resolve with no pending wait",
		log.FieldPromptID, promptID)
}

// Forget removes the wait for promptID without signalling it. Called by the
// retry loop when its budget is exhausted (no ACK arrived) so the entry does
// not leak. No-op if the wait was already resolved/removed.
func (r *AckRegistry) Forget(promptID string) {
	r.mu.Lock()
	delete(r.waiters, promptID)
	r.mu.Unlock()
}

// HandleAck is the entry point for a backend's Event ingress: when an
// protocol.TypeAck Event arrives, the backend calls this to resolve the matching
// EmitTerminal wait. promptID is the ACK's PromptID (the wait key).
func (r *AckRegistry) HandleAck(promptID string) {
	r.Resolve(promptID)
}

// WaitFor blocks until either the ACK for promptID arrives (Resolve closes the
// channel) or the timeout elapses. Returns nil on ACK, context.DeadlineExceeded
// on timeout. The caller is responsible for Forget-ing the entry after this
// returns (whether ACK or timeout) so a straggler ACK does not leak.
func (r *AckRegistry) WaitFor(promptID string, timeout time.Duration) error {
	ch := r.Arm(promptID)
	select {
	case <-ch:
		return nil
	case <-time.After(timeout):
		return context.DeadlineExceeded
	}
}

// Close drains every pending wait so EmitTerminal callers blocked in WaitFor
// unblock on shutdown. Each gets a timeout result (no ACK will ever arrive).
func (r *AckRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for promptID, ch := range r.waiters {
		close(ch)
		delete(r.waiters, promptID)
	}
}

// IsAckEvent reports whether ev is a terminal-delivery ACK the registry owns.
// Convenience for backend Event dispatch loops: `if ar.IsAckEvent(ev) { ar.HandleAck(ev.PromptID); continue }`.
func (r *AckRegistry) IsAckEvent(ev *protocol.Event) bool {
	return ev != nil && ev.Type == protocol.TypeAck
}
