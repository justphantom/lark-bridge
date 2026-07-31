package bridgebase

import (
	"context"
	"errors"
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
//     EmitTerminal goroutines blocked forever. Waiters woken by Close get
//     ErrAckRegistryClosed — NOT a success — so a terminal control in flight
//     during shutdown is never misread as delivered (which would skip the
//     retry accounting and inflate the ACK-confirmed metric).
type AckRegistry struct {
	mu      sync.Mutex
	waiters map[string]chan struct{}
	// done is closed by Close; waiters select on it alongside their per-wait
	// channel so Close can wake them with a distinct error instead of
	// closing their channel (which WaitFor cannot tell apart from Resolve).
	done   chan struct{}
	closed bool
	logger *log.Logger
}

// ErrAckRegistryClosed is returned by WaitFor when Close drains the registry
// while a wait is pending: the ACK can never arrive because the Core is
// shutting down. It is deliberately NOT nil and NOT context.DeadlineExceeded,
// so callers can distinguish "shutdown" from both "delivered" and "timed out".
var ErrAckRegistryClosed = errors.New("bridgebase: ack registry closed")

// NewAckRegistry builds an empty registry. logger is used for debug traces of
// late/duplicate ACKs; nil → no-op.
func NewAckRegistry(logger *log.Logger) *AckRegistry {
	if logger == nil {
		logger = log.Nop()
	}
	return &AckRegistry{
		waiters: make(map[string]chan struct{}),
		done:    make(chan struct{}),
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

// WaitFor blocks until the ACK for promptID arrives (Resolve closes the
// channel), the timeout elapses, or the registry is Closed. Returns nil on
// ACK, context.DeadlineExceeded on timeout, ErrAckRegistryClosed on shutdown.
// The caller is responsible for Forget-ing the entry after this returns
// (whether ACK or timeout) so a straggler ACK does not leak.
func (r *AckRegistry) WaitFor(promptID string, timeout time.Duration) error {
	ch := r.Arm(promptID)
	select {
	case <-ch:
		return nil
	case <-r.done:
		return ErrAckRegistryClosed
	case <-time.After(timeout):
		return context.DeadlineExceeded
	}
}

// Close drains every pending wait so EmitTerminal callers blocked in WaitFor
// unblock on shutdown. Each gets ErrAckRegistryClosed (no ACK will ever
// arrive) — distinct from a delivered ACK so shutdown is never misreported
// as confirmed delivery. Idempotent.
func (r *AckRegistry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.closed = true
	close(r.done)
	clear(r.waiters)
}

// IsAckEvent reports whether ev is a terminal-delivery ACK the registry owns.
// Convenience for backend Event dispatch loops: `if ar.IsAckEvent(ev) { ar.HandleAck(ev.PromptID); continue }`.
func (r *AckRegistry) IsAckEvent(ev *protocol.Event) bool {
	return ev != nil && ev.Type == protocol.TypeAck
}
