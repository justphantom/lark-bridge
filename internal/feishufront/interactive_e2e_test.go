package feishufront

import (
	"context"
	"fmt"
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
	mu        sync.Mutex
	sends     []sentCard
	textSends []sentText
	updates   []updatedCard
	nextID    int
	cardErr   error // if set, SendCard returns it (simulates a Feishu rejection)
	updateErr error // if set, UpdateCard returns it (simulates a withdrawn card)
}

type sentCard struct {
	chatID    string
	card      []byte
	replyToID string
}
type sentText struct {
	chatID    string
	text      string
	replyToID string
}
type updatedCard struct {
	messageID string
	card      []byte
}

func (f *fakeSink) SendCard(_ context.Context, chatID string, card []byte, replyToID string) (feishu.CardRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cardErr != nil {
		return feishu.CardRef{}, f.cardErr
	}
	f.nextID++
	f.sends = append(f.sends, sentCard{chatID: chatID, card: card, replyToID: replyToID})
	return feishu.CardRef{MessageID: "om_" + itoa(f.nextID)}, nil
}

func (f *fakeSink) UpdateCard(_ context.Context, messageID, _ string, card []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, updatedCard{messageID: messageID, card: card})
	return f.updateErr
}

// UpdateCardVerified records into the same updates slice as UpdateCard: the
// dispatcher-level fakes have no real Feishu to bounce a PATCH off, so they
// treat the verified path as a plain update for assertion purposes. The Bot's
// own verify loop is exercised in internal/feishu.
func (f *fakeSink) UpdateCardVerified(ctx context.Context, messageID, cardID string, card []byte) error {
	return f.UpdateCard(ctx, messageID, cardID, card)
}

// SendText records a plain-text fallback send so tests can assert the
// result-card-rejected fallback delivered the reply as text.
func (f *fakeSink) SendText(_ context.Context, chatID, text, replyToID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.textSends = append(f.textSends, sentText{chatID: chatID, text: text, replyToID: replyToID})
	return "om_" + itoa(f.nextID), nil
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

	client, err := backendrpc.Connect(backendrpc.ConnectOptions{BackendID: defaultBackend, BackendType: "opencode", FrontendURL: ts.URL})
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
	disp.turns.Start("msg-1", chatID, "", "", backendID)
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

// TestPermissionCardAction_ButtonClick verifies a TypePermission card renders
// buttons (not a dropdown) and a click routes through DispatchCardAction
// carrying the option's Value as both Choice and Choices[0], so consumers
// using PickAnswerValue read it without a form submit.
func TestPermissionCardAction_ButtonClick(t *testing.T) {
	const backendID = "opencode-perm"
	disp, sink, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_perm"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	permCtrl := &protocol.Control{
		Type:   protocol.TypePermission,
		ChatID: chatID,
		Permission: &protocol.PermissionPayload{
			RequestID: "req-perm",
			PromptID:  "msg-perm",
			Message:   "请求执行 bash",
			Options: []protocol.PermissionOption{
				{Label: "允许", Value: "allow"},
				{Label: "拒绝", Value: "deny"},
			},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}
	if sent := string(sink.lastSendCard()); !strings.Contains(sent, `"kind":"permission"`) || strings.Contains(sent, "select_static") {
		t.Fatalf("permission card should render buttons, not a dropdown: %s", sent)
	}

	action := &feishu.CardAction{
		ChatID:    chatID,
		MessageID: "msg-perm",
		Value:     map[string]any{"requestID": "req-perm", "kind": "permission", "choice": "allow"},
	}
	if err := disp.DispatchCardAction(context.Background(), action); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}

	ev, err := client.RecvEvent()
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if ev.Type != protocol.TypeAnswer || ev.Answer.RequestID != "req-perm" {
		t.Fatalf("unexpected answer event: %+v", ev)
	}
	if ev.Answer.Choice != "allow" {
		t.Fatalf("Choice = %q, want allow", ev.Answer.Choice)
	}
	if len(ev.Answer.Choices) != 1 || ev.Answer.Choices[0] != "allow" {
		t.Fatalf("Choices = %v, want [allow]", ev.Answer.Choices)
	}
	sink.mu.Lock()
	last := ""
	if n := len(sink.updates); n > 0 {
		last = string(sink.updates[n-1].card)
	}
	sink.mu.Unlock()
	if !strings.Contains(last, "你选择了") {
		t.Errorf("expected submitted summary on card, got: %s", last)
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
	disp.expireInteractive("req-t", mid, "")

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
	disp.turns.Start("msg-7", chatID, progressMID, "", backendID)

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

// TestInteractiveTakeOverProgressCard pins the slash-command picker flow: a
// question carrying TakeOverProgress morphs the command's progress card into
// the picker card (UpdateCard on the progress messageID, no fresh SendCard),
// finishes the turn, and still binds requestID → that messageID so submit and
// expiry flips keep working on the same card.
func TestInteractiveTakeOverProgressCard(t *testing.T) {
	const backendID = "opencode-10"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat10"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	const progressMID = "om-progress-10"
	disp.turns.Start("msg-10", chatID, progressMID, "", backendID)

	qCtrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: chatID, PromptID: "msg-10",
		Question: &protocol.QuestionPayload{
			RequestID: "req-tk", PromptID: "msg-10", TakeOverProgress: true,
			Questions: []protocol.QuestionItem{{Label: "选择模型", Options: []string{"a", "b"}}},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: qCtrl}); err != nil {
		t.Fatalf("question: %v", err)
	}

	mid, ok := disp.turns.InteractiveMessageID("req-tk")
	if !ok {
		t.Fatal("interactive card not bound")
	}
	if mid != progressMID {
		t.Errorf("bound messageID = %q, want progress card %q", mid, progressMID)
	}
	sink.mu.Lock()
	sends := len(sink.sends)
	var progressUpdated bool
	for _, u := range sink.updates {
		if u.messageID == progressMID && strings.Contains(string(u.card), "选择模型") {
			progressUpdated = true
		}
	}
	sink.mu.Unlock()
	if sends != 0 {
		t.Errorf("sends = %d, want 0 (no fresh card)", sends)
	}
	if !progressUpdated {
		t.Error("progress card should have been updated into the question card")
	}
	if _, turnOpen := disp.turns.Get("msg-10"); turnOpen {
		t.Error("turn should be finished after takeover")
	}

	// Submit still works on the same card.
	if err := disp.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: chatID, MessageID: progressMID,
		Value:     map[string]any{"requestID": "req-tk", "kind": "question"},
		FormValue: map[string]any{"q_0": "a"},
	}); err != nil {
		t.Fatalf("DispatchCardAction: %v", err)
	}
	sink.mu.Lock()
	var submitted bool
	for _, u := range sink.updates {
		if u.messageID == progressMID && strings.Contains(string(u.card), "已回答") {
			submitted = true
		}
	}
	sink.mu.Unlock()
	if !submitted {
		t.Error("submitted flip should target the taken-over progress card")
	}
}

// TestInteractiveTakeOverFallbackNoTurn verifies a TakeOverProgress question
// with no open turn ships a fresh standalone card exactly like before.
func TestInteractiveTakeOverFallbackNoTurn(t *testing.T) {
	const backendID = "opencode-11"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat11"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	qCtrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: chatID, PromptID: "msg-11",
		Question: &protocol.QuestionPayload{
			RequestID: "req-nf", PromptID: "msg-11", TakeOverProgress: true,
			Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: qCtrl}); err != nil {
		t.Fatalf("question: %v", err)
	}
	sink.mu.Lock()
	sends := len(sink.sends)
	sink.mu.Unlock()
	if sends != 1 {
		t.Errorf("sends = %d, want 1 (standalone fallback)", sends)
	}
}

// TestInteractiveMultipleCardsInOneTurn verifies that several permission/question
// cards emitted during the same turn each get their own standalone message.
// Regression guard: no shared requestID collision or progress-card takeover
// should swallow a later interactive card.
func TestInteractiveMultipleCardsInOneTurn(t *testing.T) {
	const backendID = "opencode-9"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat9"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	const progressMID = "om-progress"
	disp.turns.Start("msg-9", chatID, progressMID, "", backendID)

	seenMIDs := make(map[string]bool)
	for i := range 3 {
		reqID := "req-multi-" + itoa(i)
		qCtrl := &protocol.Control{
			Type: protocol.TypeQuestion, ChatID: chatID, PromptID: "msg-9",
			Question: &protocol.QuestionPayload{
				RequestID: reqID,
				PromptID:  "msg-9",
				Questions: []protocol.QuestionItem{{Label: "q" + itoa(i), Options: []string{"a", "b"}}},
			},
		}
		if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: qCtrl}); err != nil {
			t.Fatalf("question %d: %v", i, err)
		}
		mid, ok := disp.turns.InteractiveMessageID(reqID)
		if !ok {
			t.Fatalf("question %d not bound", i)
		}
		if mid == progressMID {
			t.Fatalf("question %d reused progress messageID %q", i, progressMID)
		}
		if seenMIDs[mid] {
			t.Fatalf("question %d reused messageID %q from an earlier card", i, mid)
		}
		seenMIDs[mid] = true
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

	if sends != 3 {
		t.Errorf("sends = %d, want 3", sends)
	}
	if progressOverwritten {
		t.Error("progress card must not receive UpdateCard from interactive cards")
	}
}

// lastUpdateFor returns the bytes of the most recent UpdateCard for messageID,
// or nil if none was recorded. Used by the submit→finalize lifecycle test to
// inspect the latest frame a specific card was flipped to.
func lastUpdateFor(updates []updatedCard, messageID string) []byte {
	for i := len(updates) - 1; i >= 0; i-- {
		if updates[i].messageID == messageID {
			return updates[i].card
		}
	}
	return nil
}

// countUpdatesFor reports how many UpdateCard calls targeted messageID. Used
// by the submit double-PATCH test to assert both the immediate and the
// delayed fallback PATCH landed on the same card.
func countUpdatesFor(updates []updatedCard, messageID string) int {
	n := 0
	for _, u := range updates {
		if u.messageID == messageID {
			n++
		}
	}
	return n
}

// TestInteractiveSubmittedThenFinalized pins the full mid-turn gate lifecycle:
// emit → submit (✓ echo + 处理中) → turn result → the SAME card advances to
// finalized (✓ echo PRESERVED + 已完成). Prior to the fix, submit deleted the
// cache + binding, so finalizeLinkedInteractive was a no-op and the card stuck
// on amber "处理中" forever — never reaching a terminal green state.
func TestInteractiveSubmittedThenFinalized(t *testing.T) {
	const backendID = "opencode-fin"
	disp, sink, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()
	// Drain the answer the submit forwards so it does not back up.
	go func() { _, _ = client.RecvEvent() }()

	chatID := "oc_chat_fin"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	const progressMID = "om-progress-fin"
	disp.turns.Start("msg-fin", chatID, progressMID, "", backendID)

	// Mid-turn permission gate: standalone card (no TakeOverProgress).
	permCtrl := &protocol.Control{
		Type: protocol.TypePermission, ChatID: chatID, PromptID: "msg-fin",
		Permission: &protocol.PermissionPayload{
			RequestID: "req-fin", PromptID: "msg-fin",
			Message: "执行 make test？",
			Options: []protocol.PermissionOption{
				{Label: "允许", Value: "allow"},
				{Label: "拒绝", Value: "deny"},
			},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("permission emit: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-fin")
	if mid == "" {
		t.Fatal("interactive card not bound after emit")
	}

	// User clicks 允许 → submitted flip on the SAME card.
	if err := disp.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: chatID, MessageID: mid,
		Value: map[string]any{"requestID": "req-fin", "kind": "permission", "choice": "allow"},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	sink.mu.Lock()
	submittedCard := lastUpdateFor(sink.updates, mid)
	sink.mu.Unlock()
	if submittedCard == nil || !strings.Contains(string(submittedCard), "你选择了") {
		t.Errorf("submitted card should carry the '你选择了' echo; got %s", submittedCard)
	}
	if submittedCard == nil || !strings.Contains(string(submittedCard), "处理中") {
		t.Errorf("submitted card footer should read 处理中; got %s", submittedCard)
	}
	// Binding MUST still be present after submit (the fix keeps it so finalize
	// can advance the card).
	if _, ok := disp.turns.InteractiveMessageID("req-fin"); !ok {
		t.Error("binding must survive submit so finalize can advance the card")
	}

	// Turn completes → finalize must advance the SAME card to 已完成.
	resCtrl := &protocol.Control{
		Type: protocol.TypeResult, ChatID: chatID, PromptID: "msg-fin",
		Result: &protocol.ResultPayload{Text: "done"},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: resCtrl}); err != nil {
		t.Fatalf("result emit: %v", err)
	}
	sink.mu.Lock()
	finalCard := lastUpdateFor(sink.updates, mid)
	sink.mu.Unlock()
	if finalCard == nil || !strings.Contains(string(finalCard), "已完成") {
		t.Errorf("finalized card footer should read 已完成; got %s", finalCard)
	}
	if finalCard == nil || !strings.Contains(string(finalCard), "你选择了") {
		t.Errorf("finalized card must PRESERVE the '你选择了' echo (C5); got %s", finalCard)
	}
	if finalCard != nil && strings.Contains(string(finalCard), "本轮已完成") {
		t.Errorf("finalized card must NOT prepend '本轮已完成' when a ✓ echo exists; got %s", finalCard)
	}
	// Binding released after finalize.
	if _, ok := disp.turns.InteractiveMessageID("req-fin"); ok {
		t.Error("binding should be released after finalize")
	}
}

// TestSubmit_DelayedFallbackPatchResends pins the double-PATCH hardening (R3):
// the submit path PATCHes the submitted card immediately AND re-sends the same
// bytes past Feishu's click-handling window, so an immediate PATCH silently
// reverted by the platform still lands the grey-out + "已提交". Asserts both
// PATCHes target the same card and the delayed frame keeps buttons disabled.
func TestSubmit_DelayedFallbackPatchResends(t *testing.T) {
	const backendID = "opencode-fb2"
	disp, sink, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()
	go func() { _, _ = client.RecvEvent() }() // drain the forwarded answer

	// Shrink the click-handling window so the test does not wait 5s.
	disp.cardPatchDelay = 10 * time.Millisecond

	chatID := "oc_chat_fb2"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	const progressMID = "om-progress-fb2"
	disp.turns.Start("msg-fb2", chatID, progressMID, "", backendID)

	permCtrl := &protocol.Control{
		Type: protocol.TypePermission, ChatID: chatID, PromptID: "msg-fb2",
		Permission: &protocol.PermissionPayload{
			RequestID: "req-fb2", PromptID: "msg-fb2",
			Message: "执行 make test？",
			Options: []protocol.PermissionOption{
				{Label: "允许", Value: "allow"},
				{Label: "拒绝", Value: "deny"},
			},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("permission emit: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-fb2")
	if mid == "" {
		t.Fatal("interactive card not bound after emit")
	}

	if err := disp.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: chatID, MessageID: mid,
		Value: map[string]any{"requestID": "req-fb2", "kind": "permission", "choice": "allow"},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait well past the click-handling window so the delayed fallback fires.
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return countUpdatesFor(sink.updates, mid) >= 2
	})

	sink.mu.Lock()
	last := lastUpdateFor(sink.updates, mid)
	sink.mu.Unlock()
	if last == nil {
		t.Fatal("no update recorded for the submitted card")
	}
	if !strings.Contains(string(last), "你选择了") {
		t.Errorf("delayed PATCH should carry the submitted echo; got %s", last)
	}
	if !strings.Contains(string(last), `"disabled":true`) {
		t.Errorf("delayed PATCH should leave buttons disabled; got %s", last)
	}
}

// TestSubmit_DelayedFallbackSkippedAfterFinalize verifies the delayed-PATCH
// guard: when the turn finalises before the fallback fires, the binding is
// gone and the delayed PATCH is skipped so it cannot regress the terminal
// green frame back to the grey submitted card.
func TestSubmit_DelayedFallbackSkippedAfterFinalize(t *testing.T) {
	const backendID = "opencode-fb3"
	disp, sink, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()
	go func() { _, _ = client.RecvEvent() }()

	// Finalise lands synchronously well before this window elapses.
	disp.cardPatchDelay = 40 * time.Millisecond

	chatID := "oc_chat_fb3"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	const progressMID = "om-progress-fb3"
	disp.turns.Start("msg-fb3", chatID, progressMID, "", backendID)

	permCtrl := &protocol.Control{
		Type: protocol.TypePermission, ChatID: chatID, PromptID: "msg-fb3",
		Permission: &protocol.PermissionPayload{
			RequestID: "req-fb3", PromptID: "msg-fb3",
			Message: "执行 make test？",
			Options: []protocol.PermissionOption{
				{Label: "允许", Value: "allow"},
				{Label: "拒绝", Value: "deny"},
			},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("permission emit: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-fb3")
	if mid == "" {
		t.Fatal("interactive card not bound after emit")
	}

	// Submit schedules the delayed fallback at +40ms.
	if err := disp.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: chatID, MessageID: mid,
		Value: map[string]any{"requestID": "req-fb3", "kind": "permission", "choice": "allow"},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Finalise NOW (synchronous) — before the 40ms window elapses.
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: &protocol.Control{
		Type: protocol.TypeResult, ChatID: chatID, PromptID: "msg-fb3",
		Result: &protocol.ResultPayload{Text: "done"},
	}}); err != nil {
		t.Fatalf("result emit: %v", err)
	}

	// Wait past the window so the delayed goroutine runs (and must skip).
	time.Sleep(120 * time.Millisecond)

	sink.mu.Lock()
	last := lastUpdateFor(sink.updates, mid)
	sink.mu.Unlock()
	if last == nil || !strings.Contains(string(last), "已完成") {
		t.Errorf("finalized card must remain terminal; delayed PATCH regressed it: %s", last)
	}
}

// TestSubmit_DelayedFallbackSkippedAfterNoticeUpdate verifies the delayed-PATCH
// guard also holds when the terminal frame is a TypeNotice patching the card in
// place (UpdateMessageID) rather than a turn Result — the path /clean's
// EmitCardUpdate takes. Before the fix, sendNoticeControl did not release the
// interactive binding, so the fallback re-sent the grey submitted bytes and
// overwrote the green result card, stranding it on "你选择了" forever.
func TestSubmit_DelayedFallbackSkippedAfterNoticeUpdate(t *testing.T) {
	const backendID = "omp-notice"
	disp, sink, router, client, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()
	go func() { _, _ = client.RecvEvent() }() // drain the forwarded answer

	// The notice patch lands synchronously well before this window elapses.
	disp.cardPatchDelay = 40 * time.Millisecond

	chatID := "oc_chat_notice"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}

	// A /clean permission card ships STANDALONE: the slash command
	// returned Handled before opening a progress turn, so TakeOverProgress
	// is unset and PromptID is empty. confirm/cancel map to the generic
	// "确认"/"取消" echo via choiceLabel.
	permCtrl := &protocol.Control{
		Type: protocol.TypePermission, ChatID: chatID,
		Permission: &protocol.PermissionPayload{
			RequestID: "req-notice",
			Message:   "即将删除当前目录下 2 个会话（当前绑定的会话已保留）。确认继续？",
			Options: []protocol.PermissionOption{
				{Label: "确认删除", Value: "confirm"},
				{Label: "取消", Value: "cancel"},
			},
		},
	}
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: permCtrl}); err != nil {
		t.Fatalf("permission emit: %v", err)
	}
	mid, _ := disp.turns.InteractiveMessageID("req-notice")
	if mid == "" {
		t.Fatal("interactive card not bound after emit")
	}

	// User clicks 确认删除 → submitted flip (✓ 你选择了「确认」 + 处理中) AND
	// arms the delayed fallback PATCH at +40ms.
	if err := disp.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: chatID, MessageID: mid,
		Value: map[string]any{"requestID": "req-notice", "kind": "permission", "choice": "confirm"},
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Backend resolves the answer, deletes, and patches the SAME card with a
	// success notice — the /clean result path. This must release the
	// interactive binding so the armed fallback PATCH skips.
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{
			Level:           "success",
			Title:           "清理完成",
			Message:         "已删除 2 个会话。",
			UpdateMessageID: mid,
		},
	}}); err != nil {
		t.Fatalf("notice emit: %v", err)
	}

	// Wait past the window so the delayed goroutine runs (and must skip).
	time.Sleep(120 * time.Millisecond)

	sink.mu.Lock()
	last := lastUpdateFor(sink.updates, mid)
	sink.mu.Unlock()
	if last == nil || !strings.Contains(string(last), "清理完成") {
		t.Errorf("terminal notice must remain; delayed PATCH regressed it: %s", last)
	}
	if last != nil && strings.Contains(string(last), "你选择了") {
		t.Errorf("delayed PATCH overwrote the terminal notice with submitted bytes: %s", last)
	}
	// Binding released after the terminal notice patch.
	if _, ok := disp.turns.InteractiveMessageID("req-notice"); ok {
		t.Error("binding should be released after the terminal notice patch")
	}
}

// TestSendResult_CardRejectedFallsBackToText pins the production fix for the
// silent "card not sent" bug: when Feishu rejects the result CARD's content
// (ErrCode 11310 — too many tables, surfaced as feishu.ErrCardContentRejected),
// sendResult retries the reply as a plain-text message so it is not lost.
func TestSendResult_CardRejectedFallsBackToText(t *testing.T) {
	const backendID = "opencode-fb"
	disp, sink, router, _, _, cleanup := wireFrontend(t, backendID)
	defer cleanup()

	chatID := "oc_chat_fb"
	if err := router.Set(chatID, backendID); err != nil {
		t.Fatal(err)
	}
	const progressMID = "om-progress-fb"
	disp.turns.Start("msg-fb", chatID, progressMID, "", backendID)

	// Simulate Feishu rejecting the result card (e.g. reply had too many tables).
	sink.mu.Lock()
	sink.cardErr = fmt.Errorf("%w: Code=230099, ext=ErrCode: 11310; ErrMsg: card table number over limit", feishu.ErrCardContentRejected)
	sink.mu.Unlock()

	replyText := "| col |\n|----|\n| a |\n| b |\n\n这是最终回复正文。"
	if err := disp.DispatchControl(context.Background(), RoutedControl{BackendID: backendID, Control: &protocol.Control{
		Type:     protocol.TypeResult,
		PromptID: "msg-fb",
		ChatID:   chatID,
		Result:   &protocol.ResultPayload{Text: replyText},
	}}); err != nil {
		t.Fatalf("DispatchControl result: %v", err)
	}

	// The card send failed; the reply must have been delivered as plain text.
	sink.mu.Lock()
	texts := append([]sentText(nil), sink.textSends...)
	cardSends := len(sink.sends)
	sink.mu.Unlock()
	if cardSends != 0 {
		t.Errorf("expected the card send to be rejected (0 successful sends), got %d", cardSends)
	}
	if len(texts) != 1 {
		t.Fatalf("expected exactly one SendText fallback, got %d: %+v", len(texts), texts)
	}
	if texts[0].chatID != chatID {
		t.Errorf("text fallback chatID = %q, want %q", texts[0].chatID, chatID)
	}
	if !strings.Contains(texts[0].text, "这是最终回复正文") {
		t.Errorf("text fallback should carry the reply text; got %q", texts[0].text)
	}
}

// TestSendInteractive_QuestionUpdateRefreshesSameCard verifies the multi-round
// picker refresh path (the /send directory browser): a TypeQuestion carrying
// UpdateMessageID PATCHes that existing card in place (no fresh SendCard),
// re-binds the new requestID to the card, and evicts the prior round's binding
// so the new round owns the card with no leak. The PATCH itself is delayed
// past Feishu's click window (here cardPatchDelay≈0), so the test polls for it.
func TestSendInteractive_QuestionUpdateRefreshesSameCard(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetCardPatchDelay(1 * time.Nanosecond)

	// Simulate round 1: a prior picker already owns om_picker under req-r1
	// (the progress card morphed into the picker on the first AskAndWait).
	d.turns.BindInteractive("req-r1", "om_picker", "", "")
	d.cardMu.Lock()
	d.cards["req-r1"] = []byte("round1")
	d.cardMu.Unlock()

	// Round 2: backend asks to refresh the SAME card with a new option set
	// under a fresh requestID (so clicks on the old options cannot collide).
	ctrl := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: "oc_c",
		Question: &protocol.QuestionPayload{
			RequestID:       "req-r2",
			Questions:       []protocol.QuestionItem{{Label: "选择", Options: []string{"📄 a", "📁 b/"}}},
			UpdateMessageID: "om_picker",
		},
	}
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: ctrl}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}

	// Synchronous invariants (the delayed PATCH goroutine does not affect these):
	// no fresh SendCard; the new requestID owns the card; the prior one is gone.
	if sends, _ := sink.counts(); sends != 0 {
		t.Errorf("expected 0 SendCard for an in-place refresh, got %d", sends)
	}
	if mid, ok := d.turns.InteractiveMessageID("req-r2"); !ok || mid != "om_picker" {
		t.Errorf("req-r2 should bind to om_picker, got (%q,%v)", mid, ok)
	}
	if _, ok := d.turns.InteractiveMessageID("req-r1"); ok {
		t.Error("req-r1 should be evicted by the refresh (only req-r2 owns the card)")
	}
	d.cardMu.Lock()
	_, hasNew := d.cards["req-r2"]
	_, hasOld := d.cards["req-r1"]
	d.cardMu.Unlock()
	if !hasNew {
		t.Error("new card bytes should be cached under req-r2")
	}
	if hasOld {
		t.Error("old round-1 card bytes should be evicted from the cache")
	}

	// The Feishu PATCH is fired by a delayed goroutine; poll for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		hit := false
		for _, u := range sink.updates {
			if u.messageID == "om_picker" {
				hit = true
				break
			}
		}
		sink.mu.Unlock()
		if hit {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("delayed UpdateCard on om_picker never fired within 2s")
}

// TestDispatchCardAction_MultiRoundSkipsSubmitFlip pins the anti-flicker fix:
// when a click arrives AFTER a newer round's binding was registered for the
// same card but BEFORE the delayed refresh PATCH landed, the submitted-state
// flip must be SKIPPED (it would be reverted by Feishu's click window →
// bounce-back, then the refresh lands → a third state = flicker). The click
// must still be forwarded to the backend, and the submitted bytes are stashed
// for the flush in sendInteractive.
func TestDispatchCardAction_MultiRoundSkipsSubmitFlip(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetCardPatchDelay(1 * time.Hour) // refresh PATCH never lands during the test

	// Round 1 card (pre-click requestID still owns the cached bytes).
	q1 := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: "oc_c",
		Question: &protocol.QuestionPayload{
			RequestID: "req-r1",
			Questions: []protocol.QuestionItem{{Label: "选择文件", Options: []string{"📁 sub/", "📄 a.txt"}}},
		},
	}
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: q1}); err != nil {
		t.Fatalf("round1: %v", err)
	}
	mid, _ := d.turns.InteractiveMessageID("req-r1")

	// Round 2 refresh registers its binding synchronously but its PATCH sleeps.
	q2 := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: "oc_c",
		Question: &protocol.QuestionPayload{
			RequestID:       "req-r2",
			Questions:       []protocol.QuestionItem{{Label: "选择文件", Options: []string{"📄 b.txt"}}},
			UpdateMessageID: mid,
		},
	}
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "claude-1", Control: q2}); err != nil {
		t.Fatalf("round2: %v", err)
	}
	sink.mu.Lock()
	updatesBefore := len(sink.updates)
	sink.mu.Unlock()

	// The user clicks the OLD card (form value carries round-1's requestID).
	if err := d.DispatchCardAction(context.Background(), &feishu.CardAction{
		ChatID: "oc_c", MessageID: mid,
		Value:     map[string]any{"requestID": "req-r1", "kind": "question"},
		FormValue: map[string]any{"q_0": "📁 sub/"},
	}); err != nil {
		t.Fatalf("click: %v", err)
	}

	// Anti-flicker: NO immediate submitted PATCH went out.
	sink.mu.Lock()
	updatesAfter := len(sink.updates)
	sink.mu.Unlock()
	if updatesAfter != updatesBefore {
		t.Errorf("skipped flip should emit no PATCH, got %d new updates", updatesAfter-updatesBefore)
	}
	// The submitted bytes are stashed for the flush path.
	d.cardMu.Lock()
	_, stashed := d.pendingSubmits[mid]
	d.cardMu.Unlock()
	if !stashed {
		t.Error("submitted bytes should be stashed in pendingSubmits for the flush")
	}
}
