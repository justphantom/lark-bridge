package bridgebase

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestStaticOptions_ReturnsList verifies StaticOptions wraps a fixed list
// into the listFn shape AskAndWait expects (no I/O, ctx is decorative).
func TestStaticOptions_ReturnsList(t *testing.T) {
	fn := StaticOptions([]string{"a", "b", "c"})
	got, err := fn(context.Background())
	if err != nil {
		t.Fatalf("StaticOptions: %v", err)
	}
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("got=%v, want [a b c]", got)
	}
}

// TestStaticOptions_Empty verifies an empty option slice flows through
// (the picker caller must handle len==0 as "nothing to choose", but the
// wrapper itself should not panic or synthesize a placeholder).
func TestStaticOptions_Empty(t *testing.T) {
	fn := StaticOptions(nil)
	got, err := fn(context.Background())
	if err != nil {
		t.Fatalf("StaticOptions(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got=%v, want empty", got)
	}
}

// TestPickAnswerValue_CustomWins verifies a custom-typed value overrides a
// selected option — the user explicitly typed something not in the list.
func TestPickAnswerValue_CustomWins(t *testing.T) {
	ans := &protocol.AnswerPayload{Choices: []string{"listed"}, Custom: "typed"}
	if got := PickAnswerValue(ans); got != "typed" {
		t.Errorf("custom should win; got=%q", got)
	}
}

// TestPickAnswerValue_FirstChoice verifies a single-select answer carries
// its value at Choices[0].
func TestPickAnswerValue_FirstChoice(t *testing.T) {
	ans := &protocol.AnswerPayload{Choices: []string{"only"}}
	if got := PickAnswerValue(ans); got != "only" {
		t.Errorf("got=%q, want only", got)
	}
}

// TestPickAnswerValue_Nil verifies a nil AnswerPayload yields "" rather
// than panicking (callers feed inbound answers directly).
func TestPickAnswerValue_Nil(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("panicked on nil: %v", r)
		}
	}()
	if got := PickAnswerValue(nil); got != "" {
		t.Errorf("got=%q, want empty", got)
	}
}

// TestPickAnswerValue_Empty verifies an empty AnswerPayload yields "".
func TestPickAnswerValue_Empty(t *testing.T) {
	if got := PickAnswerValue(&protocol.AnswerPayload{}); got != "" {
		t.Errorf("got=%q, want empty", got)
	}
}

// TestNewRequestID verifies the id is non-empty, has the documented prefix,
// and two consecutive calls differ (so a stale card click cannot collide
// with a fresh picker).
func TestNewRequestID(t *testing.T) {
	a, err := newRequestID()
	if err != nil {
		t.Fatalf("newRequestID: %v", err)
	}
	if !strings.HasPrefix(a, "q-") {
		t.Errorf("id=%q, want q- prefix", a)
	}
	if len(a) <= 2 {
		t.Errorf("id=%q too short", a)
	}
	b, _ := newRequestID()
	if a == b {
		t.Errorf("two ids identical: %q (must be unguessable)", a)
	}
}

// TestAskAndWait_ReturnsMessageID verifies the answer's MessageID is surfaced
// to the caller so a picker can update the original question card.
func TestAskAndWait_ReturnsMessageID(t *testing.T) {
	answers := NewAnswerBroker()
	emit := func(context.Context, string, *protocol.Control) error { return nil }

	done := make(chan struct{})
	var gotValue, gotMessageID string
	go func() {
		defer close(done)
		gotValue, gotMessageID, _ = AskAndWait(context.Background(), answers, emit, "chat-1", "", "模型", "选择模型", StaticOptions([]string{"a", "b"}), false)
	}()

	reqID := ""
	for reqID == "" {
		ids := answers.PendingIDs()
		if len(ids) > 0 {
			reqID = ids[0]
		}
	}
	answers.Deliver(reqID, &protocol.AnswerPayload{RequestID: reqID, ChatID: "chat-1", MessageID: "om_card", Choices: []string{"b"}})
	<-done

	if gotValue != "b" {
		t.Errorf("value = %q, want b", gotValue)
	}
	if gotMessageID != "om_card" {
		t.Errorf("messageID = %q, want om_card", gotMessageID)
	}
}

// TestAskAndWait_TakeOverProgress verifies the picker card is marked for
// progress-card takeover and carries the caller's replyToID as its promptID,
// so the frontend can morph the command's progress card into the picker card
// (one-card flow).
func TestAskAndWait_TakeOverProgress(t *testing.T) {
	answers := NewAnswerBroker()
	var emitted *protocol.Control
	var gotPromptID string
	emit := func(_ context.Context, promptID string, c *protocol.Control) error {
		emitted = c
		gotPromptID = promptID
		return nil
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		AskAndWait(context.Background(), answers, emit, "chat-1", "om_cmd", "模型", "选择模型", StaticOptions([]string{"a"}), false)
	}()

	reqID := ""
	for reqID == "" {
		if ids := answers.PendingIDs(); len(ids) > 0 {
			reqID = ids[0]
		}
	}
	answers.Deliver(reqID, &protocol.AnswerPayload{RequestID: reqID, ChatID: "chat-1", Choices: []string{"a"}})
	<-done

	if emitted == nil {
		t.Fatal("no question control emitted")
	}
	if !emitted.Question.TakeOverProgress {
		t.Error("TakeOverProgress should be set on picker cards")
	}
	if gotPromptID != "om_cmd" {
		t.Errorf("promptID = %q, want om_cmd", gotPromptID)
	}
}

// TestAskAndWait_TruncatesOptionsAtCap verifies an oversized option list is
// truncated to maxQuestionOptions before reaching the card. Feishu rejects
// larger cards with ErrCode 11310 "element exceeds the limit".
func TestAskAndWait_TruncatesOptionsAtCap(t *testing.T) {
	big := make([]string, maxQuestionOptions+50)
	for i := range big {
		big[i] = "opt-" + strconv.Itoa(i)
	}

	var emitted *protocol.Control
	emit := func(_ context.Context, _ string, c *protocol.Control) error {
		emitted = c
		return nil
	}

	answers := NewAnswerBroker()
	done := make(chan struct{})
	go func() {
		defer close(done)
		AskAndWait(context.Background(), answers, emit, "chat-cap", "", "模型", "选择模型", StaticOptions(big), true)
	}()

	reqID := ""
	for reqID == "" {
		if ids := answers.PendingIDs(); len(ids) > 0 {
			reqID = ids[0]
		}
	}
	answers.Deliver(reqID, &protocol.AnswerPayload{RequestID: reqID, ChatID: "chat-cap", Choices: []string{"opt-0"}})
	<-done

	if emitted == nil || len(emitted.Question.Questions) != 1 {
		t.Fatalf("emitted = %+v, want one question", emitted)
	}
	got := emitted.Question.Questions[0].Options
	if len(got) != maxQuestionOptions {
		t.Fatalf("options len = %d, want %d", len(got), maxQuestionOptions)
	}
	// Truncation keeps the prefix in list order.
	if got[0] != "opt-0" || got[maxQuestionOptions-1] != "opt-"+strconv.Itoa(maxQuestionOptions-1) {
		t.Errorf("prefix not preserved: first=%q last=%q", got[0], got[maxQuestionOptions-1])
	}
}
