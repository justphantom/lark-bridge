// Regression tests for generalizing the /send bounce-back fix
// (markCardTerminal / isCardTerminal) to every card writer that can race a
// delayed or TTL-driven PATCH.
package feishufront

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestExpireInteractive_DropsWhenCardAlreadyTerminal pins the terminal-state
// guard on the TTL path: if a delayed writer (or finalize) already landed the
// terminal frame before the expiry timer fires, the expired PATCH must be
// dropped instead of regressing the card back to "已自动失效".
func TestExpireInteractive_DropsWhenCardAlreadyTerminal(t *testing.T) {
	const backendID = "omp-term-exp"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat_term_exp"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	permCtrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: chatID,
		Question: &protocol.QuestionPayload{RequestID: "req-term-exp", PromptID: "msg-1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-term-exp")
	if mid == "" {
		t.Fatal("interactive binding missing after send")
	}

	// A terminal frame landed first (e.g. a delayed refresh raced the TTL and
	// another writer finished the card). The expiry timer fires late.
	disp.markCardTerminal(mid)
	before := len(sink.updates)
	disp.expireInteractive("req-term-exp", mid, "")

	if got := len(sink.updates); got != before {
		t.Errorf("expired PATCH must be dropped on a terminal card; updates went %d → %d", before, got)
	}
}

// TestDelayedRefresh_DropsAfterExpire pins the opposite race direction
// (Scenario E): a /send-style delayed refresh is armed, the TTL expires the
// card ("已失效"), and the refresh must then self-abort rather than revive the
// dead card back to a live picker.
func TestDelayedRefresh_DropsAfterExpire(t *testing.T) {
	const backendID = "omp-exp-refresh"
	disp, sink, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()
	go func() { _, _ = client.RecvEvent() }() // drain the forwarded answer

	disp.cardPatchDelay = 40 * time.Millisecond

	chatID := "oc_chat_exp_refresh"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	permCtrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: chatID,
		Question: &protocol.QuestionPayload{RequestID: "req-exp-refresh", PromptID: "msg-1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-exp-refresh")
	if mid == "" {
		t.Fatal("interactive binding missing after send")
	}

	// Click arms the delayed refresh PATCH at +40ms (sendInteractiveCard's
	// same-card path).
	if err := disp.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: chatID, MessageID: mid,
		Value: map[string]any{"requestID": "req-exp-refresh", "kind": "question", "choice": "a"},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// The TTL fires while the refresh is still sleeping.
	disp.expireInteractive("req-exp-refresh", mid, "")

	// Wait well past the window so the delayed goroutine runs and must skip.
	time.Sleep(120 * time.Millisecond)

	sink.mu.Lock()
	last := lastUpdateFor(sink.updates, mid)
	sink.mu.Unlock()
	if last == nil || !strings.Contains(string(last), "已自动失效") {
		t.Errorf("expired card must remain terminal; delayed refresh revived it: %s", last)
	}
}
