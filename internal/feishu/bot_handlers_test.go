package feishu

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// logNop returns a no-op logger for tests that build a Bot by hand.
func logNop() *log.Logger { return log.Nop() }

// TestBuildCardActionForwardsFormValue pins the lark event → CardAction
// mapping for a form submit: action.value (button custom data) and
// action.form_value (form component values) must both reach the dispatcher,
// which reads FormValue to recover question answers.
func TestBuildCardActionForwardsFormValue(t *testing.T) {
	ev := &lark.CardActionEvent{
		EventID:   "ev-1",
		ChatID:    "oc_c",
		MessageID: "om_m",
		Operator:  lark.CardActionOperator{OpenID: "ou_x"},
		Action: lark.CardActionPayload{
			Value:     map[string]any{"requestID": "r1", "kind": "question"},
			FormValue: map[string]any{"q_0": "a", "custom_0": "note"},
		},
	}
	got := buildCardAction(ev)
	if got.EventID != "ev-1" || got.ChatID != "oc_c" || got.MessageID != "om_m" {
		t.Fatalf("identity not forwarded: %+v", got)
	}
	if got.UserOpenID != "ou_x" {
		t.Fatalf("UserOpenID = %q", got.UserOpenID)
	}
	if got.Value["requestID"] != "r1" {
		t.Fatalf("Value not forwarded: %v", got.Value)
	}
	if got.FormValue["q_0"] != "a" || got.FormValue["custom_0"] != "note" {
		t.Fatalf("FormValue not forwarded: %v", got.FormValue)
	}
}

// TestHandleCardAction_DropsEmptyOperator verifies the guard: a card action
// with an empty operator open_id is dropped (nil) before reaching the
// registered handler — downstream permission/reply addressing needs a real
// user id.
func TestHandleCardAction_DropsEmptyOperator(t *testing.T) {
	var called atomic.Bool
	b := &Bot{logger: logNop()}
	h := CardActionHandler(func(_ context.Context, _ *CardAction) error {
		called.Store(true)
		return nil
	})
	b.onCardAction.Store(&h)
	err := b.handleCardAction(context.Background(), &lark.CardActionEvent{
		EventID: "ev", Operator: lark.CardActionOperator{OpenID: ""},
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if called.Load() {
		t.Fatal("handler should not be called for empty operator")
	}
}

// TestShouldExitUnhealthy pins the watchdog decision boundary: never exit
// before the bot has been healthy once, never exit inside the startup grace
// window, and exit only once the gap since last-healthy exceeds fatalAfter.
func TestShouldExitUnhealthy(t *testing.T) {
	started := time.Unix(1000, 0)
	fatalAfter := 2 * time.Minute
	healthy1 := started.Add(10 * time.Second) // connected once at t=10s
	tests := []struct {
		name             string
		now, lastHealthy time.Time
		want             bool
	}{
		{"never healthy", started.Add(5 * time.Minute), time.Time{}, false},
		{"within grace window", started.Add(30 * time.Second), healthy1, false},
		{"recently healthy", started.Add(5 * time.Minute), started.Add(5*time.Minute - 30*time.Second), false},
		{"stale past fatalAfter", started.Add(5 * time.Minute), healthy1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShouldExitUnhealthy(tc.now, tc.lastHealthy, started, fatalAfter); got != tc.want {
				t.Errorf("ShouldExitUnhealthy = %v, want %v", got, tc.want)
			}
		})
	}
}
