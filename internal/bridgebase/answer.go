package bridgebase

import (
	"sync"

	"github.com/hu/lark-bridge/internal/protocol"
)

// AnswerBroker routes an interactive card's answer back to the goroutine that
// emitted the Question control. A picker registers a one-shot channel under a
// requestID, emits the card, and blocks on the channel; the inbound TypeAnswer
// event delivers the answer via Deliver. Drain (called at shutdown) closes
// every pending slot so a blocked picker returns immediately rather than
// waiting out the ask timeout.
//
// Each bridge's Handler exposes thin same-named wrappers
// (registerAnswer/cancelAnswer/deliverAnswer/drainAnswers) over a held
// *AnswerBroker so call sites and tests stay unchanged while the routing logic
// — lock, map, double-answer semantics — lives in exactly one place.
type AnswerBroker struct {
	mu      sync.Mutex
	pending map[string]chan *protocol.AnswerPayload
}

// NewAnswerBroker returns a ready-to-use AnswerBroker.
func NewAnswerBroker() *AnswerBroker {
	return &AnswerBroker{pending: make(map[string]chan *protocol.AnswerPayload)}
}

// Register reserves a slot for an interactive card's answer under requestID and
// returns the channel that will receive it. Registration fails (ok=false) when
// requestID is already pending; the caller should generate a fresh id.
func (b *AnswerBroker) Register(requestID string) (<-chan *protocol.AnswerPayload, bool) {
	ch := make(chan *protocol.AnswerPayload, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.pending[requestID]; exists {
		return nil, false
	}
	b.pending[requestID] = ch
	return ch, true
}

// Cancel removes a pending slot without delivering an answer. Safe to call
// after Deliver (it is a no-op if the slot is already gone).
func (b *AnswerBroker) Cancel(requestID string) {
	b.mu.Lock()
	delete(b.pending, requestID)
	b.mu.Unlock()
}

// Deliver routes an inbound answer to the goroutine waiting on requestID, if
// any. Returns false when no waiter exists (the card may have timed out on the
// backend side already); the answer is then discarded.
func (b *AnswerBroker) Deliver(requestID string, ans *protocol.AnswerPayload) bool {
	b.mu.Lock()
	ch, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- ans:
	default:
		// Channel has buffer 1; a drop means a double-answer (e.g. user
		// clicked twice before dedup caught it). Keep the first.
	}
	return true
}

// Drain closes every pending answer slot so any blocked picker returns
// immediately with a cancellation. Called by Close.
func (b *AnswerBroker) Drain() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.pending {
		close(ch)
		delete(b.pending, id)
	}
}

// PendingIDs returns the requestIDs of every currently-registered slot, for
// tests that need the id of a slot a picker just registered. Order is not
// meaningful.
func (b *AnswerBroker) PendingIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.pending))
	for id := range b.pending {
		ids = append(ids, id)
	}
	return ids
}
