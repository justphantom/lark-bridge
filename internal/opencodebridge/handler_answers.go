package opencodebridge

import (
	"github.com/hu/lark-bridge/internal/protocol"
)

// registerAnswer reserves a slot for an interactive card's answer under
// requestID and returns the channel that will receive it. The returned cancel
// func must be called when the caller stops waiting (success, timeout, or
// error) to remove the slot. If requestID is already pending, registration
// fails (the caller should generate a fresh id).
func (h *Handler) registerAnswer(requestID string) (<-chan *protocol.AnswerPayload, bool) {
	ch := make(chan *protocol.AnswerPayload, 1)
	h.answerMu.Lock()
	defer h.answerMu.Unlock()
	if _, exists := h.pendingAnswers[requestID]; exists {
		return nil, false
	}
	h.pendingAnswers[requestID] = ch
	return ch, true
}

// cancelAnswer removes a pending slot without delivering an answer. Safe to
// call after deliverAnswer (it is a no-op if the slot is already gone).
func (h *Handler) cancelAnswer(requestID string) {
	h.answerMu.Lock()
	delete(h.pendingAnswers, requestID)
	h.answerMu.Unlock()
}

// deliverAnswer routes an inbound answer to the goroutine waiting on
// requestID, if any. Returns false when no waiter exists (the card may have
// timed out on the backend side already); the answer is then discarded.
func (h *Handler) deliverAnswer(requestID string, ans *protocol.AnswerPayload) bool {
	h.answerMu.Lock()
	ch, ok := h.pendingAnswers[requestID]
	if ok {
		delete(h.pendingAnswers, requestID)
	}
	h.answerMu.Unlock()
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

// drainAnswers closes every pending answer slot so any askAndWait goroutine
// still blocked returns immediately with a cancellation. Called by Close.
func (h *Handler) drainAnswers() {
	h.answerMu.Lock()
	defer h.answerMu.Unlock()
	for id, ch := range h.pendingAnswers {
		close(ch)
		delete(h.pendingAnswers, id)
	}
}
