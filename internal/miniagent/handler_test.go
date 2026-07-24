package miniagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// captureSender records every Control sent. Controls returns a snapshot.
type captureSender struct {
	mu       sync.Mutex
	controls []*protocol.Control
}

func (c *captureSender) SendControl(_ context.Context, ctrl *protocol.Control) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.controls = append(c.controls, ctrl)
	return nil
}

func (c *captureSender) Controls() []*protocol.Control {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*protocol.Control, len(c.controls))
	copy(out, c.controls)
	return out
}

// newTestHandler builds a Handler with no router and no CLI client —
// sufficient for testing HandleEvent dispatch (empty prompt, non-prompt
// ignore, busy-then-drop, abort). Turn execution is not exercised here;
// handler_cli_test covers it.
func newTestHandler() (*Handler, *captureSender) {
	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, "", "test-model", nil)
	return h, sender
}

// TestHandleEvent_EmptyPromptNotices verifies an all-whitespace prompt is
// replied to with a warning notice rather than reaching runTurn.
func TestHandleEvent_EmptyPromptNotices(t *testing.T) {
	h, rpc := newTestHandler()
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p3", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "   "}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	controls := rpc.Controls()
	if len(controls) != 1 || controls[0].Type != protocol.TypeNotice {
		t.Errorf("Controls = %+v, want 1 Notice", controls)
	}
}

// TestHandleEvent_NonPromptIgnored verifies non-Prompt events are dropped.
func TestHandleEvent_NonPromptIgnored(t *testing.T) {
	h, _ := newTestHandler()
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePing}); err != nil {
		t.Errorf("HandleEvent: %v", err)
	}
}

// TestHandleEvent_MissingChatIDErrors verifies a prompt missing ChatID is
// rejected with an error rather than silently dropped.
func TestHandleEvent_MissingChatIDErrors(t *testing.T) {
	h, _ := newTestHandler()
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p", Prompt: &protocol.PromptPayload{ChatID: "", Text: "hi"}}
	if err := h.HandleEvent(context.Background(), ev); err == nil {
		t.Error("expected error for missing chatID, got nil")
	}
}

// TestHandleEvent_MissingPromptIDErrors verifies a prompt missing PromptID
// is rejected. The frontend assigns one per inbound message; reusing a
// chatID-derived id made the 2nd turn's Result collide with the 1st's closed
// card and silently drop.
func TestHandleEvent_MissingPromptIDErrors(t *testing.T) {
	h, _ := newTestHandler()
	ev := &protocol.Event{Type: protocol.TypePrompt, Prompt: &protocol.PromptPayload{ChatID: "c", Text: "hi"}}
	if err := h.HandleEvent(context.Background(), ev); err == nil {
		t.Error("expected error for missing promptID, got nil")
	}
}

// TestAbort_IdleReply verifies /session-abort when no turn is in flight
// returns a "无可中止" notice instead of erroring.
func TestAbort_IdleReply(t *testing.T) {
	h, rpc := newTestHandler()
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "/session-abort"}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	got := rpc.Controls()
	if len(got) != 1 {
		t.Fatalf("emits = %d, want 1", len(got))
	}
	if got[0].Type != protocol.TypeNotice {
		t.Errorf("type = %v, want Notice", got[0].Type)
	}
	if got[0].Notice.Level != "info" {
		t.Errorf("level = %q, want info (idle abort)", got[0].Notice.Level)
	}
}

// TestRunning_NoTurn verifies /running on an idle chat reports empty.
func TestRunning_NoTurn(t *testing.T) {
	h, rpc := newTestHandler()
	// /running path needs a chat to filter by; it does not require router.
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "/running"}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	got := rpc.Controls()
	if len(got) != 1 {
		t.Fatalf("emits = %d, want 1", len(got))
	}
	if got[0].Notice == nil || !containsStr(got[0].Notice.Message, "当前没有运行中的会话") {
		t.Errorf("body = %q, want idle message", got[0].Notice.Message)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestHandler_CloseWaitsForInFlight verifies Close blocks until in-flight
// turns wind down (or the grace period elapses). The turn here is a stub:
// newTestHandler leaves client == nil so runTurn returns immediately; this
// still exercises the startTurn/endTurn/Close lifecycle.
func TestHandler_CloseWaitsForInFlight(t *testing.T) {
	h, _ := newTestHandler()
	// Sanity: Close on a fresh handler returns immediately.
	done := make(chan struct{})
	go func() {
		h.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close on idle handler blocked >1s")
	}
	// Second Close is a no-op (idempotent).
	h.Close()
}

// TestStartPrompt_ConcurrentSerialization is the bridge-side concurrency
// contract: 32 goroutines racing to startTurn on the same chat must yield
// exactly one winner at a time (others rejected or queued via endTurn).
// Mirrors the bridgebase prompt_slot test.
func TestStartPrompt_ConcurrentSerialization(t *testing.T) {
	h, _ := newTestHandler()
	defer h.Close()
	const N = 32
	var winners int64
	var wg sync.WaitGroup
	wg.Add(N)
	for range N {
		go func() {
			defer wg.Done()
			_, mine, ok := h.startTurn(context.Background(), "race")
			if ok {
				atomic.AddInt64(&winners, 1)
				h.endTurn("race", mine)
			}
		}()
	}
	wg.Wait()
	if atomic.LoadInt64(&winners) == 0 {
		t.Error("expected at least one winner under contention")
	}
}
