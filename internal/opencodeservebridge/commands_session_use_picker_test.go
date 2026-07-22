package opencodeservebridge

import (
	"context"
	"fmt"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// TestCmdSessionUse_Picker_Success drives the async picker: /session-use
// returns Handled immediately; delivering the "2. …" label switches the
// binding to the second sorted session.
func TestCmdSessionUse_Picker_Success(t *testing.T) {
	now := time.Now().UnixMilli()
	agent := &useFakeAgent{
		sessions: useSessions(now),
		statuses: map[string]oc.SessionStatus{},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/proj", "", "", "")

	res, err := h.cmdSessionUse(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !res.Handled {
		t.Error("picker path should return Handled=true immediately")
	}

	reqID := waitPending(t, h, time.Second)
	label := fmt.Sprintf("2. 旧会话 · %s", formatTime(now-3600000))
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{label}, MessageID: "msg-1"})

	if got := waitSessionID(t, r, "chat-1", "s2", 2*time.Second); got != "s2" {
		t.Errorf("SessionID = %q, want s2 after picking %q", got, label)
	}
}

// TestCmdSessionUse_Picker_BusyRecheck verifies the picker's apply-time
// re-check: a session that turns busy after being listed is refused and the
// binding stays put.
func TestCmdSessionUse_Picker_BusyRecheck(t *testing.T) {
	now := time.Now().UnixMilli()
	agent := &useFakeAgent{
		sessions: useSessions(now),
		statuses: map[string]oc.SessionStatus{},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	reqID := waitPending(t, h, time.Second)
	agent.setStatus("s2", "busy") // turns busy while the card is open
	label := fmt.Sprintf("2. 旧会话 · %s", formatTime(now-3600000))
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{label}, MessageID: "msg-1"})
	waitAnswerConsumed(t, h, 2*time.Second)
	time.Sleep(50 * time.Millisecond) // let the goroutine act before asserting

	b, _ := r.Lookup("chat-1")
	if b.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1 (busy re-check must refuse the switch)", b.SessionID)
	}
}

// TestCmdSessionUse_Picker_AllBusy verifies that with every session busy the
// picker reports the busy count and never pops a card.
func TestCmdSessionUse_Picker_AllBusy(t *testing.T) {
	now := time.Now().UnixMilli()
	agent := &useFakeAgent{
		sessions: useSessions(now),
		statuses: map[string]oc.SessionStatus{
			"s1": {Type: "busy"},
			"s2": {Type: "busy"},
		},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/proj", "", "", "")

	res, err := h.cmdSessionUse(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !res.Handled {
		t.Error("picker path should return Handled=true immediately")
	}
	// The goroutine exits after noticing all sessions are busy; no answer
	// slot may appear and the binding must not move.
	time.Sleep(100 * time.Millisecond)
	if ids := h.Answers.PendingIDs(); len(ids) != 0 {
		t.Errorf("PendingIDs = %v, want none (all busy → no card)", ids)
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1", b.SessionID)
	}
}

// waitSessionID polls the router until the chat's SessionID equals want or
// the timeout elapses. Used to synchronise on the async picker goroutine.
func waitSessionID(t *testing.T, r *router.Router, chatID, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, ok := r.Lookup(chatID)
		if ok && b.SessionID == want {
			return b.SessionID
		}
		time.Sleep(2 * time.Millisecond)
	}
	b, _ := r.Lookup(chatID)
	return b.SessionID
}
