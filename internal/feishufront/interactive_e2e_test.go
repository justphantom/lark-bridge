package feishufront

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/protocol"
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
		Type: protocol.TypeQuestion, ChatID: chatID,
		Question: &protocol.QuestionPayload{RequestID: "req-3", PromptID: "msg-1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}},
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
// dispatcher forwards an Answer Event carrying Choices + Custom + MessageID.
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
	if ev.Answer.MessageID != "msg-1" {
		t.Fatalf("MessageID = %q, want msg-1", ev.Answer.MessageID)
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
		Type: protocol.TypeQuestion, ChatID: chatID,
		Question: &protocol.QuestionPayload{RequestID: "req-t", PromptID: "msg-1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}},
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

// TestInteractiveFinalizedOnResult covers a standalone interactive card (no
// in-flight progress card to take over): when a result control lands for a
// prompt that still has a pending standalone interactive card, the card is
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

	// No turn in flight: the question card ships standalone (fresh SendCard).
	permCtrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: chatID, PromptID: "msg-6",
		Question: &protocol.QuestionPayload{RequestID: "req-f", PromptID: "msg-6", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}},
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
		t.Error("expected standalone interactive card finalised with '本轮已完成'")
	}
	if _, ok := disp.turns.InteractiveMessageID("req-f"); ok {
		t.Error("interactive binding should be released after result")
	}
}

// TestInteractiveSendsNewCard pins the post-takeover behaviour: a question
// arriving mid-turn ships a fresh standalone card with its own messageID. The
// in-flight progress card is never touched (no UpdateCard on its messageID).
// The result card later ships as another fresh SendCard and the interactive
// binding is released.
func TestInteractiveSendsNewCard(t *testing.T) {
	const backendID = "opencode-7"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat7"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	const progressMID = "om-progress"
	disp.turns.Start("msg-7", chatID, progressMID, backendID)

	permCtrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: chatID, PromptID: "msg-7",
		Question: &protocol.QuestionPayload{RequestID: "req-r", PromptID: "msg-7", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("question: %v", err)
	}
	// The question card must ship as a fresh SendCard with its own messageID.
	mid, _ := disp.turns.InteractiveMessageID("req-r")
	if mid == "" {
		t.Fatal("interactive card not bound")
	}
	if mid == progressMID {
		t.Fatalf("interactive messageID = %q, must NOT equal progress messageID %q", mid, progressMID)
	}
	sink.mu.Lock()
	sends := len(sink.sends)
	var progressOverwritten bool
	for _, u := range sink.updates {
		if u.messageID == progressMID {
			progressOverwritten = true
		}
	}
	sink.mu.Unlock()
	if sends == 0 {
		t.Error("expected a fresh SendCard for the question, got 0 sends")
	}
	if progressOverwritten {
		t.Error("progress card must NOT receive any UpdateCard from the question")
	}

	resCtrl := &protocol.Control{
		Type: protocol.TypeResult, ChatID: chatID, PromptID: "msg-7",
		Result: &protocol.ResultPayload{Text: "done"},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: resCtrl}); err != nil {
		t.Fatalf("result: %v", err)
	}
	sink.mu.Lock()
	lastSend := ""
	if len(sink.sends) > 0 {
		lastSend = string(sink.sends[len(sink.sends)-1].card)
	}
	sink.mu.Unlock()
	if !strings.Contains(lastSend, "done") {
		t.Errorf("result should ship as a fresh SendCard carrying the result text, got: %s", lastSend)
	}
	if _, ok := disp.turns.InteractiveMessageID("req-r"); ok {
		t.Error("interactive binding should be released after result")
	}
}

// TestQuestionSubmit_ShowsAnswerOnCard verifies that submitting a question
// form flips the card to show "✓ 已回答: <answer>" — the user sees what was
// picked at a glance instead of a generic "已提交" placeholder.
func TestQuestionSubmit_ShowsAnswerOnCard(t *testing.T) {
	const backendID = "opencode-8"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat8"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	qCtrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: chatID, PromptID: "msg-8",
		Question: &protocol.QuestionPayload{RequestID: "req-a", PromptID: "msg-8",
			Questions: []protocol.QuestionItem{{Label: "选什么", Options: []string{"选项A", "选项B"}}}},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: qCtrl}); err != nil {
		t.Fatalf("question: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-a")
	if mid == "" {
		t.Fatal("interactive card not bound")
	}

	if err := disp.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: chatID, MessageID: mid,
		Value:     map[string]any{"requestID": "req-a", "kind": "question"},
		FormValue: map[string]any{"q_0": "选项A"},
	}); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}

	sink.mu.Lock()
	var submittedCard string
	for _, u := range sink.updates {
		if u.messageID == mid {
			submittedCard = string(u.card)
		}
	}
	sink.mu.Unlock()
	if !strings.Contains(submittedCard, "已回答") {
		t.Errorf("submitted card should contain '已回答', got: %s", submittedCard)
	}
	if !strings.Contains(submittedCard, "选项A") {
		t.Errorf("submitted card should contain the answer '选项A', got: %s", submittedCard)
	}
}
