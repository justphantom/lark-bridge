package feishufront

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
func (p *pickerRouter) Touch(string)            {}

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
// card when at least one backend is online, and arms the TTL expiry timer.
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
	// The picker's TTL timer must be armed so an unclicked card expires.
	d.cardMu.Lock()
	timers := len(d.pickerTimers)
	cards := len(d.pickerCards)
	d.cardMu.Unlock()
	if timers != 1 {
		t.Errorf("want 1 picker timer armed, got %d", timers)
	}
	if cards != 1 {
		t.Errorf("want 1 picker card cached, got %d", cards)
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
// end-to-end: the chat is rebound, the card is refreshed in place, the TTL
// timer is cancelled, and — critically — nothing is forwarded to any backend.
func TestDispatchCardAction_BackendPicker_Switches(t *testing.T) {
	reg := NewBackendRegistry()
	conn := reg.Register("claude-1", "claude")
	reg.Register("opencode-1", "opencode")
	rt := &pickerRouter{current: "claude-1"}
	sink := &fakeSink{}
	d := NewDispatcher(sink, reg, NewTurnManager(), rt)
	// Shrink the click-handling delay so the test does not wait 5s.
	d.cardPatchDelay = 10 * time.Millisecond
	// Pre-arm a picker TTL so the test can assert the click cancels it.
	d.pickerCards["om_card"] = []byte("{}")
	d.pickerTimers["om_card"] = time.AfterFunc(time.Hour, func() {})

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
	// Success path delays the PATCH past Feishu's click-handling window.
	// Shrink the delay so the test does not wait 5s.
	d.cardPatchDelay = 10 * time.Millisecond
	// Wait for the goroutine to land the green outcome card.
	waitFor(t, func() bool {
		_, updates := sink.counts()
		return updates == 1
	})
	sends, updates := sink.counts()
	if sends != 0 || updates != 1 {
		t.Errorf("want 0 sends + 1 delayed update, got %d sends + %d updates", sends, updates)
	}
	// Click cancels the TTL flip so a late expiry cannot overwrite the result.
	d.cardMu.Lock()
	timers := len(d.pickerTimers)
	cards := len(d.pickerCards)
	d.cardMu.Unlock()
	if timers != 0 || cards != 0 {
		t.Errorf("want picker TTL cancelled (0 timers, 0 cards), got %d timers, %d cards", timers, cards)
	}
}

// TestRenderBackendOutcome_Success verifies the success-state picker card has
// a green header, confirmation body, and every backend button disabled.
func TestRenderBackendOutcome_Success(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	reg.Register("opencode-1", "opencode")
	rt := &pickerRouter{current: "opencode-1"}
	d := NewDispatcher(&fakeSink{}, reg, NewTurnManager(), rt)

	card, err := d.renderBackendOutcome("oc_x", "opencode-1", "opencode", "success", "已切换后端", "当前后端: opencode-1（opencode）")
	if err != nil {
		t.Fatalf("renderBackendOutcome: %v", err)
	}
	s := string(card)
	if !strings.Contains(s, "已切换后端") {
		t.Errorf("result card missing title: %s", s)
	}
	if !strings.Contains(s, "当前后端: opencode-1（opencode）") {
		t.Errorf("result card missing confirmation body: %s", s)
	}
	if !strings.Contains(s, `"template":"green"`) {
		t.Errorf("result card header not green: %s", s)
	}
	if !strings.Contains(s, `"disabled":true`) {
		t.Errorf("result card buttons not disabled: %s", s)
	}
	if !strings.Contains(s, "✓ opencode-1") {
		t.Errorf("current backend not marked ✓: %s", s)
	}
}

// TestRenderBackendOutcome_Failure verifies the failure-state picker card has
// a red header, the failure body, and every backend button disabled.
func TestRenderBackendOutcome_Failure(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	rt := &pickerRouter{current: "claude-1"}
	d := NewDispatcher(&fakeSink{}, reg, NewTurnManager(), rt)

	card, err := d.renderBackendOutcome("oc_x", "ghost", "", "error", "后端离线", "backend ghost 已不在线。发送 /backend 重新选择。")
	if err != nil {
		t.Fatalf("renderBackendOutcome: %v", err)
	}
	s := string(card)
	if !strings.Contains(s, `"template":"red"`) {
		t.Errorf("failure card header not red: %s", s)
	}
	if !strings.Contains(s, "后端离线") {
		t.Errorf("failure card missing title: %s", s)
	}
	if !strings.Contains(s, "已不在线") {
		t.Errorf("failure card missing body: %s", s)
	}
	if !strings.Contains(s, `"disabled":true`) {
		t.Errorf("failure card buttons not disabled: %s", s)
	}
}

// TestDispatchCardAction_BackendPicker_OfflineRejected verifies that clicking
// a backend that went offline between render and click flips the SAME picker
// card to a red failure state in place (one-card principle) instead of
// emitting a separate notice.
func TestDispatchCardAction_BackendPicker_OfflineRejected(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	rt := &pickerRouter{current: "claude-1"}
	sink := &fakeSink{}
	d := NewDispatcher(sink, reg, NewTurnManager(), rt)
	d.cardPatchDelay = 10 * time.Millisecond

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
	// Wait for the delayed PATCH to land the red failure card in place.
	waitFor(t, func() bool {
		_, updates := sink.counts()
		return updates == 1
	})
	sends, updates := sink.counts()
	if sends != 0 || updates != 1 {
		t.Errorf("want 0 sends + 1 delayed update, got %d sends + %d updates", sends, updates)
	}
	s := string(sink.updates[0].card)
	if !strings.Contains(s, "离线") {
		t.Errorf("want failure card mentioning offline, got %s", s)
	}
	if !strings.Contains(s, `"template":"red"`) {
		t.Errorf("want red header on failure card: %s", s)
	}
}

// waitFor polls cond every 5ms for up to 1s. Used to synchronise with the
// delayed-PATCH goroutine without sleeping a fixed duration (which would
// make tests either flaky or slow).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true within 1s")
}

// TestExpirePicker verifies the TTL timer flips a cached picker card to its
// expired form (grey + "已失效") via UpdateCard, then drops the cache.
func TestExpirePicker(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	sink := &fakeSink{}
	d := NewDispatcher(sink, reg, NewTurnManager(), &pickerRouter{})
	// Render and cache a real picker so RenderInteractiveExpired has bytes to
	// mutate; fakeSink.SendCard returns om_1 for the first call.
	picker, err := d.renderBackendPicker("oc_x")
	if err != nil {
		t.Fatalf("renderBackendPicker: %v", err)
	}
	d.armPickerExpiry("om_card", picker)

	d.expirePicker("om_card")

	if len(sink.updates) != 1 {
		t.Fatalf("want 1 UpdateCard on expiry, got %d", len(sink.updates))
	}
	s := string(sink.updates[0].card)
	if !strings.Contains(s, "已失效") {
		t.Errorf("expired card missing 已失效 marker: %s", s)
	}
	d.cardMu.Lock()
	timers := len(d.pickerTimers)
	cards := len(d.pickerCards)
	d.cardMu.Unlock()
	if timers != 0 || cards != 0 {
		t.Errorf("want expiry to drop cache, got %d timers + %d cards", timers, cards)
	}
}

// TestExpirePicker_NoopAfterClick verifies a late expiry (racing a click that
// already cancelled the timer) does not overwrite the outcome card.
func TestExpirePicker_NoopAfterClick(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), &pickerRouter{})
	d.pickerCards["om_card"] = []byte("{}")
	d.pickerTimers["om_card"] = time.AfterFunc(time.Hour, func() {})

	if _, ok := d.cancelPickerExpiry("om_card"); !ok {
		t.Fatalf("cancelPickerExpiry should report a cached card")
	}
	d.expirePicker("om_card") // cache already cleared → no-op
	if len(sink.updates) != 0 {
		t.Errorf("late expiry overwrote the outcome: %v", sink.updates)
	}
}
