package claudebridge

import (
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// newInteractiveTestHandler builds a Handler with a fresh in-memory router and
// no rpc/agent, suitable for testing the pending-answer routing directly.
func newInteractiveTestHandler(t *testing.T) *Handler {
	t.Helper()
	r, err := router.New(nil, "", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	return NewWithLogger(r, nil, nil, HandlerConfig{DefaultDirectory: t.TempDir()}, log.Nop())
}

// TestRegisterDeliverCancel exercises the pending-answer routing: register →
// deliver wakes the waiter; deliver with no waiter returns false; double
// register fails; cancel removes the slot. This is the infrastructure the
// interactive pickers depend on, independent of any specific picker command.
func TestRegisterDeliverCancel(t *testing.T) {
	h := newInteractiveTestHandler(t)

	t.Run("deliver wakes waiter", func(t *testing.T) {
		ch, ok := h.registerAnswer("req-1")
		if !ok {
			t.Fatal("register failed")
		}
		ans := &protocol.AnswerPayload{Choices: []string{"sonnet"}}
		if !h.deliverAnswer("req-1", ans) {
			t.Fatal("deliver returned false for a pending waiter")
		}
		select {
		case got := <-ch:
			if len(got.Choices) != 1 || got.Choices[0] != "sonnet" {
				t.Errorf("got %v, want Choices=[sonnet]", got)
			}
		case <-time.After(time.Second):
			t.Fatal("waiter not woken")
		}
	})

	t.Run("deliver with no waiter returns false", func(t *testing.T) {
		if h.deliverAnswer("unknown", &protocol.AnswerPayload{}) {
			t.Fatal("deliver should return false when no waiter exists")
		}
	})

	t.Run("double register fails", func(t *testing.T) {
		if _, ok := h.registerAnswer("req-2"); !ok {
			t.Fatal("first register should succeed")
		}
		if _, ok := h.registerAnswer("req-2"); ok {
			t.Fatal("second register of same id should fail")
		}
		h.cancelAnswer("req-2")
	})

	t.Run("cancel removes slot", func(t *testing.T) {
		h.registerAnswer("req-3")
		h.cancelAnswer("req-3")
		if h.deliverAnswer("req-3", &protocol.AnswerPayload{}) {
			t.Fatal("deliver after cancel should return false")
		}
	})
}

// TestDrainAnswers_OnClose verifies Close wakes every blocked waiter so the
// process can exit without leaking goroutines. Without drainAnswers a picker
// blocked on askAndWait would hang until askWaitTimeout (9 min).
func TestDrainAnswers_OnClose(t *testing.T) {
	h := newInteractiveTestHandler(t)
	ch, ok := h.registerAnswer("req-close")
	if !ok {
		t.Fatal("register failed")
	}
	// Simulate Close cancelling the app context and draining pending answers.
	// drainAnswers closes the channel; the receiver gets a zero-value nil.
	go h.Close()
	select {
	case ans, ok := <-ch:
		// drainAnswers closes the channel: ok=false, ans=nil. Either way the
		// waiter is unblocked, which is the contract.
		_ = ans
		if !ok {
			return // channel closed — waiter unblocked, no leak
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the blocked waiter — goroutine leak")
	}
}
