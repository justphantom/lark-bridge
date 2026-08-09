package feishufront

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// fakeFileSender records the last SendFile invocation and optionally returns
// an error, letting each test pin one outcome of handleFileControl.
type fakeFileSender struct {
	err      error
	calls    int
	lastName string
}

func (f *fakeFileSender) SendFile(_ context.Context, _ string, fileName string, _ io.Reader) error {
	f.calls++
	f.lastName = fileName
	return f.err
}

func newFileControl(t *testing.T, chatID, updateID string) *protocol.Control {
	t.Helper()
	return &protocol.Control{
		Type:   protocol.TypeFile,
		ChatID: chatID,
		File: &protocol.FilePayload{
			ChatID:          chatID,
			FileName:        "report.txt",
			Content:         base64.StdEncoding.EncodeToString([]byte("hello")),
			UpdateMessageID: updateID,
		},
	}
}

func TestHandleFileControl_Success_PatchesPickerCard(t *testing.T) {
	sink := &fakeSink{}
	sender := &fakeFileSender{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetFileSender(sender)

	if err := d.handleFileControl(context.Background(), newFileControl(t, "oc_c", "om_card"), "claude"); err != nil {
		t.Fatalf("handleFileControl: %v", err)
	}
	if sender.calls != 1 {
		t.Errorf("SendFile calls = %d, want 1", sender.calls)
	}
	sends, updates := sink.counts()
	if sends != 0 || updates != 1 {
		t.Errorf("sends=%d updates=%d, want 0 sends + 1 update (picker patched)", sends, updates)
	}
	sink.mu.Lock()
	card := string(sink.updates[0].card)
	sink.mu.Unlock()
	if !strings.Contains(card, "已发送") {
		t.Errorf("patched card missing 已发送:\n%s", card)
	}
}

func TestHandleFileControl_Success_NoCardIsSilent(t *testing.T) {
	// Direct /send <path> (no picker): success stays quiet — the file landing
	// in the chat is the confirmation; no card to patch and no extra notice.
	sink := &fakeSink{}
	sender := &fakeFileSender{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetFileSender(sender)

	if err := d.handleFileControl(context.Background(), newFileControl(t, "oc_c", ""), "claude"); err != nil {
		t.Fatalf("handleFileControl: %v", err)
	}
	if sender.calls != 1 {
		t.Errorf("SendFile calls = %d, want 1", sender.calls)
	}
	sends, updates := sink.counts()
	if sends != 0 || updates != 0 {
		t.Errorf("sends=%d updates=%d, want both 0 (silent success)", sends, updates)
	}
}

func TestHandleFileControl_Failure_PatchesPickerCard(t *testing.T) {
	sink := &fakeSink{}
	sender := &fakeFileSender{err: errors.New("boom")}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetFileSender(sender)

	_ = d.handleFileControl(context.Background(), newFileControl(t, "oc_c", "om_card"), "claude")
	sends, updates := sink.counts()
	if sends != 0 || updates != 1 {
		t.Errorf("sends=%d updates=%d, want 0 sends + 1 patched failure", sends, updates)
	}
	sink.mu.Lock()
	card := string(sink.updates[0].card)
	sink.mu.Unlock()
	if !strings.Contains(card, "发送失败") {
		t.Errorf("patched card missing 发送失败:\n%s", card)
	}
}

func TestHandleFileControl_Failure_StandaloneNotice(t *testing.T) {
	sink := &fakeSink{}
	sender := &fakeFileSender{err: errors.New("boom")}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetFileSender(sender)

	_ = d.handleFileControl(context.Background(), newFileControl(t, "oc_c", ""), "claude")
	sends, updates := sink.counts()
	if updates != 0 || sends != 1 {
		t.Errorf("sends=%d updates=%d, want 1 standalone failure notice", sends, updates)
	}
	if card := sink.lastSendCard(); !strings.Contains(string(card), "发送失败") {
		t.Errorf("standalone notice missing 发送失败:\n%s", card)
	}
}

func TestHandleFileControl_NoSender_ErrorNotice(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	// No SetFileSender: a TypeFile arriving must not crash; it surfaces an error.

	_ = d.handleFileControl(context.Background(), newFileControl(t, "oc_c", ""), "claude")
	sends, _ := sink.counts()
	if sends != 1 {
		t.Errorf("sends=%d, want 1 error notice (no FileSender wired)", sends)
	}
}

// TestReflectFileOutcome_EvictsInteractiveBinding pins the bounce-back fix:
// the picker card that just submitted a file pick still has an interactive
// binding armed with a delayed submit-fallback PATCH. reflectFileOutcome must
// evict that binding when it patches the terminal frame, otherwise the
// fallback (5s later) overwrites the green "已发送" card with the grey
// "已提交，正在处理…" bytes — the reported bounce-back.
func TestReflectFileOutcome_EvictsInteractiveBinding(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, nil, NewTurnManager(), nil)

	d.turns.BindInteractive("q-pick", "om_picker", "", "prompt_x")
	d.cardMu.Lock()
	d.cards["q-pick"] = []byte(`{"header":{"template":"grey"},"config":{}}`)
	d.cardMu.Unlock()

	ctrl := &protocol.Control{
		ChatID: "oc_chat",
		File:   &protocol.FilePayload{FileName: "a.txt", UpdateMessageID: "om_picker"},
	}
	d.reflectFileOutcome(context.Background(), ctrl, "", "success", "已发送", "已发送：a.txt")

	if _, ok := d.turns.InteractiveMessageID("q-pick"); ok {
		t.Error("interactive binding should be evicted so the submit fallback cannot overwrite the outcome card")
	}
	d.cardMu.Lock()
	_, cached := d.cards["q-pick"]
	d.cardMu.Unlock()
	if cached {
		t.Error("cached picker card bytes should be evicted with the binding")
	}
	sends, updates := sink.counts()
	if sends != 0 || updates != 1 {
		t.Errorf("sends=%d updates=%d, want 0 sends and 1 outcome patch", sends, updates)
	}
}

// TestDelayedSelectedPatch_DroppedAfterOutcome pins the fix for the reported
// 回弹: miniagent /send's file pick fires emitSelectedCard ("已选择 X",
// TypeQuestion+UpdateMessageID, PATCH delayed past the click window) and then
// emitSendFile (TypeFile, outcome PATCH immediate). The outcome lands FIRST and
// marks the card terminal; the still-sleeping "已选择 X" PATCH must then be
// dropped instead of reverting the green outcome frame back to the grey
// selected echo. Without the terminal guard, the delayed PATCH lands last and
// the user sees the card bounce from 已发送 back to 已选择.
func TestDelayedSelectedPatch_DroppedAfterOutcome(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	// Long enough that the outcome PATCH below lands before the delayed
	// "已选择 X" goroutine wakes, short enough the test finishes fast.
	d.SetCardPatchDelay(300 * time.Millisecond)

	// Register the picker card so the UpdateMessageID refresh path rebinds it.
	d.turns.BindInteractive("q-pick", "om_picker", "", "prompt_x")
	d.cardMu.Lock()
	d.cards["q-pick"] = []byte(`{"header":{"template":"grey"},"config":{}}`)
	d.cardMu.Unlock()

	// emitSelectedCard: "已选择 X" refresh, PATCH sleeps cardPatchDelay.
	sel := &protocol.Control{
		Type: protocol.TypeQuestion, ChatID: "oc_c",
		Question: &protocol.QuestionPayload{
			RequestID:       "q-sel",
			Questions:       []protocol.QuestionItem{{Label: "已选择 a.txt", Options: []string{"已选择 a.txt"}}},
			UpdateMessageID: "om_picker",
		},
	}
	if err := d.DispatchControl(context.Background(), RoutedControl{BackendID: "miniagent-1", Control: sel}); err != nil {
		t.Fatalf("emitSelectedCard: %v", err)
	}

	// emitSendFile outcome: lands immediately, marks om_picker terminal.
	d.reflectFileOutcome(context.Background(),
		&protocol.Control{ChatID: "oc_c", File: &protocol.FilePayload{FileName: "a.txt", UpdateMessageID: "om_picker"}},
		"", "success", "已发送", "已发送：a.txt")

	// Wait out the delayed selected PATCH window.
	time.Sleep(600 * time.Millisecond)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var outcomeSeen, selectedAfterOutcome bool
	outcomeIdx := -1
	for i, u := range sink.updates {
		if u.messageID != "om_picker" {
			continue
		}
		body := string(u.card)
		if strings.Contains(body, "已发送") {
			outcomeSeen = true
			outcomeIdx = i
		}
	}
	if !outcomeSeen {
		t.Fatal("outcome (已发送) PATCH never landed on om_picker")
	}
	for i, u := range sink.updates {
		if i > outcomeIdx && u.messageID == "om_picker" && strings.Contains(string(u.card), "已选择") {
			selectedAfterOutcome = true
		}
	}
	if selectedAfterOutcome {
		t.Error("delayed \"已选择 X\" PATCH landed AFTER the outcome — the bounce-back the terminal guard must prevent")
	}
}
