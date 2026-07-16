package claudebridge

import "github.com/hu/lark-bridge/internal/protocol"

// Interactive-card answer routing is delegated to a shared
// bridgebase.AnswerBroker held in h.answers. These thin wrappers preserve the
// Handler method names so every call site — askAndWait, the TypeAnswer event
// branch, Close — and the unit tests keep working unchanged, while the routing
// logic (lock, map, double-answer semantics) lives exactly once in bridgebase
// instead of being copy-pasted across the three bridges.

// registerAnswer reserves a slot for an interactive card's answer under
// requestID and returns the channel that will receive it. The returned cancel
// func must be called when the caller stops waiting (success, timeout, or
// error) to remove the slot. If requestID is already pending, registration
// fails (the caller should generate a fresh id).
func (h *Handler) registerAnswer(requestID string) (<-chan *protocol.AnswerPayload, bool) {
	return h.answers.Register(requestID)
}

// cancelAnswer removes a pending slot without delivering an answer. Safe to
// call after deliverAnswer (it is a no-op if the slot is already gone).
func (h *Handler) cancelAnswer(requestID string) {
	h.answers.Cancel(requestID)
}

// deliverAnswer routes an inbound answer to the goroutine waiting on
// requestID, if any. Returns false when no waiter exists (the card may have
// timed out on the backend side already); the answer is then discarded.
func (h *Handler) deliverAnswer(requestID string, ans *protocol.AnswerPayload) bool {
	return h.answers.Deliver(requestID, ans)
}

// drainAnswers closes every pending answer slot so any askAndWait goroutine
// still blocked returns immediately with a cancellation. Called by Close.
func (h *Handler) drainAnswers() {
	h.answers.Drain()
}
