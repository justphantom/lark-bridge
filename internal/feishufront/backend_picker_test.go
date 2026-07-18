package feishufront

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/justphantom/lark-bridge/internal/feishu"
)

// pickerRouter is a ChatRouter stub that records the last Set target so
// backend-picker tests can assert which backend a click bound. Resolve returns
// the current binding ("" when none), mirroring Layer1Router's empty state.
type pickerRouter struct {
	mu       sync.Mutex
	current  string
	setCalls int
}

func (p *pickerRouter) Resolve(string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.current, nil
}

func (p *pickerRouter) Set(_ string, backendID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current = backendID
	p.setCalls++
	return nil
}

func (p *pickerRouter) ChatsOf(string) []string { return nil }

// TestRenderBackendPicker verifies the picker lists every online backend as a
// button and marks the currently-bound one ✓ + disabled.
func TestRenderBackendPicker(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	reg.Register("opencode-1", "opencode")
	rt := &pickerRouter{current: "claude-1"}
	d := NewDispatcher(&fakeSink{}, reg, NewTurnManager(), rt)

	card, err := d.renderBackendPicker("oc_x")
	if err != nil {
		t.Fatalf("renderBackendPicker: %v", err)
	}
	s := string(card)
	if !strings.Contains(s, "claude-1") || !strings.Contains(s, "opencode-1") {
		t.Errorf("picker missing backend ids: %s", s)
	}
	if !strings.Contains(s, "✓ claude-1") {
		t.Errorf("current backend not marked ✓: %s", s)
	}
	if !strings.Contains(s, `"disabled":true`) {
		t.Errorf("current backend button not disabled: %s", s)
	}
}

// TestHandleBackendCommand_Picker verifies /backend sends exactly one picker
// card when at least one backend is online.
func TestHandleBackendCommand_Picker(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	sink := &fakeSink{}
	d := NewDispatcher(sink, reg, NewTurnManager(), &pickerRouter{})

	if err := d.handleBackendCommand(context.Background(), &feishu.IncomingMessage{
		ChatID: "oc_x", MessageID: "om_msg", MsgType: "text",
	}, ""); err != nil {
		t.Fatalf("handleBackendCommand: %v", err)
	}
	sends, _ := sink.counts()
	if sends != 1 {
		t.Fatalf("want 1 picker card sent, got %d", sends)
	}
	if !strings.Contains(string(sink.lastSendCard()), "选择后端") {
		t.Errorf("sent card is not a picker: %s", sink.lastSendCard())
	}
}

// TestHandleBackendCommand_NoBackends verifies /backend surfaces a notice when
// no backend is online (an empty picker would be useless).
func TestHandleBackendCommand_NoBackends(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), &pickerRouter{})

	if err := d.handleBackendCommand(context.Background(), &feishu.IncomingMessage{
		ChatID: "oc_x", MessageID: "om_msg", MsgType: "text",
	}, ""); err != nil {
		t.Fatalf("handleBackendCommand: %v", err)
	}
	if !strings.Contains(string(sink.lastSendCard()), "没有后端") {
		t.Errorf("want no-backend notice, got %s", sink.lastSendCard())
	}
}

// TestDispatchCardAction_BackendPicker_Switches drives a picker button click
// end-to-end: the chat is rebound, the card is refreshed in place, and —
// critically — nothing is forwarded to any backend.
func TestDispatchCardAction_BackendPicker_Switches(t *testing.T) {
	reg := NewBackendRegistry()
	conn := reg.Register("claude-1", "claude")
	reg.Register("opencode-1", "opencode")
	rt := &pickerRouter{current: "claude-1"}
	sink := &fakeSink{}
	d := NewDispatcher(sink, reg, NewTurnManager(), rt)

	if err := d.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID:    "oc_x",
		MessageID: "om_card",
		Value:     map[string]any{"kind": "backend", "backendID": "opencode-1"},
	}); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}
	if rt.current != "opencode-1" {
		t.Errorf("current = %q, want opencode-1", rt.current)
	}
	// A picker click must NOT be forwarded to a backend as an Answer event.
	select {
	case ev := <-conn.eventCh:
		t.Fatalf("backend received unexpected event %q", ev.Type)
	default:
	}
	// Success path refreshes the picker in place (1 update) AND sends a
	// confirmation notice (1 send) — the card refresh alone is too subtle.
	sends, updates := sink.counts()
	if sends != 1 || updates != 1 {
		t.Errorf("want 1 send + 1 update, got %d sends + %d updates", sends, updates)
	}
}

// TestDispatchCardAction_BackendPicker_OfflineRejected verifies that clicking
// a backend that went offline between render and click does not rebind and
// surfaces a warning notice instead.
func TestDispatchCardAction_BackendPicker_OfflineRejected(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	rt := &pickerRouter{current: "claude-1"}
	sink := &fakeSink{}
	d := NewDispatcher(sink, reg, NewTurnManager(), rt)

	if err := d.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID:    "oc_x",
		MessageID: "om_card",
		Value:     map[string]any{"kind": "backend", "backendID": "ghost"},
	}); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}
	if rt.current != "claude-1" {
		t.Errorf("current changed to %q on offline pick", rt.current)
	}
	// Offline → notice via SendCard (no in-place refresh).
	sends, updates := sink.counts()
	if sends != 1 || updates != 0 {
		t.Errorf("want 1 notice send + 0 updates, got %d sends + %d updates", sends, updates)
	}
	if !strings.Contains(string(sink.lastSendCard()), "离线") {
		t.Errorf("want offline notice, got %s", sink.lastSendCard())
	}
}
