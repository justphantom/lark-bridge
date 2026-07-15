package feishufront

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/feishu"
	"github.com/hu/lark-bridge/internal/protocol"
)

// fakeSink is a CardSink that records every SendCard/UpdateCard call and
// returns synthetic message ids so the dispatcher can track turns.
type fakeSink struct {
	mu      sync.Mutex
	sends   []sentCard
	updates []updatedCard
	nextID  int
}

type sentCard struct {
	chatID    string
	card      []byte
	replyToID string
}
type updatedCard struct {
	messageID string
	card      []byte
}

func (f *fakeSink) SendCard(_ context.Context, chatID string, card []byte, replyToID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.sends = append(f.sends, sentCard{chatID: chatID, card: card, replyToID: replyToID})
	return "om_" + itoa(f.nextID), nil
}

func (f *fakeSink) UpdateCard(_ context.Context, messageID string, card []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, updatedCard{messageID: messageID, card: card})
	return nil
}

func (f *fakeSink) lastSendCard() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sends) == 0 {
		return nil
	}
	return f.sends[len(f.sends)-1].card
}

// counts returns the number of recorded SendCard and UpdateCard calls.
func (f *fakeSink) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends), len(f.updates)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// wireFrontend builds a real IPCServer + Layer1Router + Dispatcher with a
// fake bot sink, and connects a real backendrpc.Client so the dispatcher can
// read Answer Events exactly as it would in production.
func wireFrontend(t *testing.T, defaultBackend string) (*Dispatcher, *fakeSink, *Layer1Router, *backendrpc.Client, *BackendRegistry, func()) {
	t.Helper()
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	router, err := NewLayer1Router("")
	if err != nil {
		ts.Close()
		t.Fatalf("router: %v", err)
	}
	turns := NewTurnManager()
	disp := NewDispatcher(sink, reg, turns, router)

	client, err := backendrpc.Connect(defaultBackend, "opencode", ts.URL, "")
	if err != nil {
		ts.Close()
		t.Fatalf("connect: %v", err)
	}
	cleanup := func() {
		client.Close()
		ts.Close()
	}
	return disp, sink, router, client, reg, cleanup
}

// TestPermissionRoundTrip drives a full permission interaction: the backend
// POSTs a PermissionRequest Control; the dispatcher renders an interactive
// card; a CardAction clicks allow; the dispatcher flips the card and forwards
// an Answer Event the backend receives.
func TestPermissionRoundTrip(t *testing.T) {
	const backendID = "opencode-1"
	disp, sink, router, client, reg, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat"
	// Bind the chat to the backend so DispatchCardAction can resolve it.
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	// 1. Backend POSTs a PermissionRequest Control.
	permCtrl := &protocol.Control{
		Type:   protocol.TypePermissionRequest,
		ChatID: chatID,
		PermissionRequest: &protocol.PermissionRequestPayload{
			RequestID: "req-1",
			PromptID:  "msg-1",
			Message:   "allow write to /tmp?",
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}

	// 2. Assert the dispatcher sent an interactive card with allow/deny.
	card := sink.lastSendCard()
	if card == nil {
		t.Fatal("expected SendCard for permission, got none")
	}
	body := string(card)
	if !strings.Contains(body, "allow") || !strings.Contains(body, "deny") || !strings.Contains(body, "req-1") {
		t.Fatalf("permission card missing allow/deny/requestID: %s", body)
	}

	// 3. Simulate a CardAction: user clicks allow.
	action := &feishu.CardAction{
		ChatID:    chatID,
		MessageID: "msg-1",
		Value:     map[string]any{"requestID": "req-1", "choice": "allow", "kind": "permission"},
	}
	if err := disp.DispatchCardAction(context.Background(), action); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}

	// 4. The interactive card must have been updated (submitted state).
	sink.mu.Lock()
	gotUpdate := len(sink.updates) > 0
	sink.mu.Unlock()
	if !gotUpdate {
		t.Fatal("expected UpdateCard to flip the interactive card to submitted")
	}
	// Answer-forwarding is covered by TestPermissionRoundTrip_AnswerForwarded.
	_ = client
	_ = reg
}

// TestPermissionRoundTrip_AnswerForwarded uses the public RecvEvent path to
// confirm the Answer Event reaches the backend.
func TestPermissionRoundTrip_AnswerForwarded(t *testing.T) {
	const backendID = "opencode-2"
	disp, _, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat2"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	// Register the turn so DispatchControl can find the progress card path
	// (not needed for permission, but harmless).
	disp.turns.Start("msg-1", chatID, "", backendID)

	permCtrl := &protocol.Control{
		Type:              protocol.TypePermissionRequest,
		ChatID:            chatID,
		PermissionRequest: &protocol.PermissionRequestPayload{RequestID: "req-2", PromptID: "msg-1", Message: "m"},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}

	action := &feishu.CardAction{
		ChatID:    chatID,
		MessageID: "msg-1",
		Value:     map[string]any{"requestID": "req-2", "choice": "allow", "kind": "permission"},
	}
	if err := disp.DispatchCardAction(context.Background(), action); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}

	ev, err := client.RecvEvent()
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if ev.Type != protocol.TypeAnswer || ev.Answer.RequestID != "req-2" || ev.Answer.Choice != "allow" {
		t.Fatalf("unexpected answer event: %+v", ev)
	}
}

// TestCardActionIdempotent verifies a duplicate CardAction (same requestID)
// is dropped after the first one.
func TestCardActionIdempotent(t *testing.T) {
	const backendID = "opencode-3"
	disp, _, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat3"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	disp.turns.Start("msg-1", chatID, "", backendID)
	disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: &protocol.Control{
		Type: protocol.TypePermissionRequest, ChatID: chatID,
		PermissionRequest: &protocol.PermissionRequestPayload{RequestID: "req-3", PromptID: "msg-1", Message: "m"},
	}})

	action := &feishu.CardAction{ChatID: chatID, MessageID: "msg-1",
		Value: map[string]any{"requestID": "req-3", "choice": "allow", "kind": "permission"}}
	disp.DispatchCardAction(context.Background(), action)
	disp.DispatchCardAction(context.Background(), action) // duplicate

	// Only the first action forwards an Answer.
	ev, err := client.RecvEvent()
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if ev.Type != protocol.TypeAnswer {
		t.Fatalf("expected answer, got %v", ev.Type)
	}
	// A second RecvEvent should block (no duplicate); confirm via timeout.
	done := make(chan struct{})
	go func() {
		client.RecvEvent()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("duplicate CardAction forwarded a second Answer")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestQuestionRoundTrip_AnswerForwarded drives a question form submit end to
// end: the backend POSTs a Question Control; the dispatcher renders a form
// card; a CardAction submits form_value (a select + a custom input); the
// dispatcher forwards an Answer Event carrying Choices + Custom.
func TestQuestionRoundTrip_AnswerForwarded(t *testing.T) {
	const backendID = "opencode-4"
	disp, _, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat4"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	qCtrl := &protocol.Control{
		Type:   protocol.TypeQuestion,
		ChatID: chatID,
		Question: &protocol.QuestionPayload{
			RequestID: "req-q",
			PromptID:  "msg-1",
			Questions: []protocol.QuestionItem{{
				Label:   "选择",
				Options: []string{"选项A", "选项B"},
				Custom:  true,
			}},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: qCtrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}

	// Simulate a form submit: q_0 carries the selected option label, custom_0
	// the free-text input (matching the renderer's name convention).
	action := &feishu.CardAction{
		ChatID:    chatID,
		MessageID: "msg-1",
		Value:     map[string]any{"requestID": "req-q", "kind": "question"},
		FormValue: map[string]any{"q_0": "选项A", "custom_0": "备注"},
	}
	if err := disp.DispatchCardAction(context.Background(), action); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}

	ev, err := client.RecvEvent()
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if ev.Type != protocol.TypeAnswer || ev.Answer.RequestID != "req-q" {
		t.Fatalf("unexpected answer event: %+v", ev)
	}
	if len(ev.Answer.Choices) != 1 || ev.Answer.Choices[0] != "选项A" {
		t.Fatalf("Choices = %v, want [选项A]", ev.Answer.Choices)
	}
	if ev.Answer.Custom != "备注" {
		t.Fatalf("Custom = %q, want 备注", ev.Answer.Custom)
	}
}

// TestInteractiveTimeout verifies the expiry path: a permission card that no
// one responds to within the TTL is flipped to its expired form and its
// binding/timer are released. The TTL timer itself is driven by calling
// expireInteractive directly (the timer's body) so the test need not wait.
func TestInteractiveTimeout(t *testing.T) {
	const backendID = "opencode-5"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat5"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	permCtrl := &protocol.Control{
		Type: protocol.TypePermissionRequest, ChatID: chatID,
		PermissionRequest: &protocol.PermissionRequestPayload{RequestID: "req-t", PromptID: "msg-1", Message: "m"},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}

	// Confirm the card and its binding/timer were registered.
	_, bound := disp.turns.InteractiveMessageID("req-t")
	if !bound {
		t.Fatal("interactive binding missing after send")
	}
	disp.cardMu.Lock()
	timerThere := disp.interactiveTimers["req-t"] != nil
	cardThere := disp.cards["req-t"] != nil
	disp.cardMu.Unlock()
	if !timerThere || !cardThere {
		t.Fatalf("timer=%v card=%v, want both registered", timerThere, cardThere)
	}

	// Resolve the real messageID, then fire the expiry callback.
	mid, _ := disp.turns.InteractiveMessageID("req-t")
	disp.expireInteractive("req-t", mid)

	// The expired card should be the last UpdateCard, carrying the notice.
	sink.mu.Lock()
	var last string
	if n := len(sink.updates); n > 0 {
		last = string(sink.updates[n-1].card)
	}
	sink.mu.Unlock()
	if !strings.Contains(last, "已自动失效") {
		t.Errorf("expected expired card, got: %s", last)
	}
	// Binding and timer must be gone.
	if _, ok := disp.turns.InteractiveMessageID("req-t"); ok {
		t.Error("binding should be cleared after expiry")
	}
	disp.cardMu.Lock()
	_, timerGone := disp.interactiveTimers["req-t"]
	_, cardGone := disp.cards["req-t"]
	disp.cardMu.Unlock()
	if timerGone || cardGone {
		t.Errorf("timer/card should be cleared after expiry")
	}
}

// TestInteractiveFinalizedOnResult verifies scheme ④: when a result control
// lands for a prompt that still has a pending interactive card, the card is
// flipped to a finalised state and its binding/timer released — it does not
// linger grey forever.
func TestInteractiveFinalizedOnResult(t *testing.T) {
	const backendID = "opencode-6"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat6"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	// A turn owns the prompt the permission card is linked to.
	disp.turns.Start("msg-6", chatID, "om-progress", backendID)

	// Backend raises a permission request mid-turn.
	permCtrl := &protocol.Control{
		Type: protocol.TypePermissionRequest, ChatID: chatID, PromptID: "msg-6",
		PermissionRequest: &protocol.PermissionRequestPayload{RequestID: "req-f", PromptID: "msg-6", Message: "m"},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("permission: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-f")
	if mid == "" {
		t.Fatal("interactive card not bound")
	}

	// Turn completes with a result control.
	resCtrl := &protocol.Control{
		Type: protocol.TypeResult, ChatID: chatID, PromptID: "msg-6",
		Result: &protocol.ResultPayload{Text: "done"},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: resCtrl}); err != nil {
		t.Fatalf("result: %v", err)
	}

	// The interactive card must have been finalised (notice prepended) and
	// unbound. Look for the finalisation notice in the UpdateCard stream.
	sink.mu.Lock()
	var seen bool
	for _, u := range sink.updates {
		if u.messageID == mid && strings.Contains(string(u.card), "本轮已完成") {
			seen = true
		}
	}
	sink.mu.Unlock()
	if !seen {
		t.Error("expected interactive card finalised with '本轮已完成'")
	}
	if _, ok := disp.turns.InteractiveMessageID("req-f"); ok {
		t.Error("interactive binding should be released after result")
	}
}
