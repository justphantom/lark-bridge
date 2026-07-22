package opencodeservebridge

import (
	"context"
	"errors"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// pickerFakeAgent is an opencodeAPI fake for the interactive picker tests. Its
// ListModels/ListAgents return canned lists; Run is unused by the
// picker path.
type pickerFakeAgent struct {
	models []string
	agents []string
}

func (pickerFakeAgent) Run(context.Context, oc.RunOptions) (<-chan oc.HighEvent, error) {
	ch := make(chan oc.HighEvent)
	close(ch)
	return ch, nil
}

func (pickerFakeAgent) AbortSession(context.Context, string) error { return nil }

func (pickerFakeAgent) ListSessions(context.Context) ([]oc.SessionInfo, error) { return nil, nil }

func (pickerFakeAgent) SessionStatuses(context.Context) (map[string]oc.SessionStatus, error) {
	return map[string]oc.SessionStatus{}, nil
}

func (pickerFakeAgent) DeleteSessionIfIdle(context.Context, string) error { return nil }

func (f pickerFakeAgent) ListModels(context.Context) ([]string, error) { return f.models, nil }

func (f pickerFakeAgent) ListAgents(context.Context) ([]string, error) { return f.agents, nil }

// failingListAgent returns an error from ListModels/ListAgents to exercise the
// error path of askAndWait (the picker goroutine logs the error and emits a
// Notice; the test asserts no pending slot is left behind).
type failingListAgent struct{}

func (failingListAgent) Run(context.Context, oc.RunOptions) (<-chan oc.HighEvent, error) {
	ch := make(chan oc.HighEvent)
	close(ch)
	return ch, nil
}

func (failingListAgent) AbortSession(context.Context, string) error { return nil }

func (failingListAgent) ListSessions(context.Context) ([]oc.SessionInfo, error) { return nil, nil }

func (failingListAgent) SessionStatuses(context.Context) (map[string]oc.SessionStatus, error) {
	return map[string]oc.SessionStatus{}, nil
}

func (failingListAgent) DeleteSessionIfIdle(context.Context, string) error { return nil }

func (failingListAgent) ListModels(context.Context) ([]string, error) {
	return nil, errors.New("provider offline")
}

func (failingListAgent) ListAgents(context.Context) ([]string, error) {
	return nil, errors.New("provider offline")
}

// TestPickAnswerValue covers the selection-extraction rule: a custom-typed
// value beats a listed pick; Choices[0] is the fallback for single-select.
func TestPickAnswerValue(t *testing.T) {
	cases := []struct {
		name string
		ans  *protocol.AnswerPayload
		want string
	}{
		{"nil answer", nil, ""},
		{"custom wins over choice", &protocol.AnswerPayload{Custom: "manual/x", Choices: []string{"p/m"}}, "manual/x"},
		{"choices fallback", &protocol.AnswerPayload{Choices: []string{"p/m"}}, "p/m"},
		{"empty everything", &protocol.AnswerPayload{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bridgebase.PickAnswerValue(c.ans); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRegisterDeliverCancel exercises the pending-answer routing directly:
// register → deliver wakes the waiter; deliver with no waiter returns false;
// cancel removes the slot.
func TestRegisterDeliverCancel(t *testing.T) {
	h, _ := newPickerHandler(t)

	t.Run("deliver wakes waiter", func(t *testing.T) {
		ch, ok := h.Answers.Register("req-1")
		if !ok {
			t.Fatal("register failed")
		}
		ans := &protocol.AnswerPayload{Choices: []string{"p/m"}}
		if !h.Answers.Deliver("req-1", ans) {
			t.Fatal("deliver returned false for a pending waiter")
		}
		select {
		case got := <-ch:
			if len(got.Choices) != 1 || got.Choices[0] != "p/m" {
				t.Errorf("got %v, want Choices=[p/m]", got)
			}
		case <-time.After(time.Second):
			t.Fatal("waiter not woken")
		}
	})

	t.Run("deliver with no waiter returns false", func(t *testing.T) {
		if h.Answers.Deliver("unknown", &protocol.AnswerPayload{}) {
			t.Fatal("deliver should return false when no waiter exists")
		}
	})

	t.Run("double register fails", func(t *testing.T) {
		if _, ok := h.Answers.Register("req-2"); !ok {
			t.Fatal("first register should succeed")
		}
		if _, ok := h.Answers.Register("req-2"); ok {
			t.Fatal("second register of same id should fail")
		}
		h.Answers.Cancel("req-2")
	})

	t.Run("cancel removes slot", func(t *testing.T) {
		h.Answers.Register("req-3")
		h.Answers.Cancel("req-3")
		if h.Answers.Deliver("req-3", &protocol.AnswerPayload{}) {
			t.Fatal("deliver after cancel should return false")
		}
	})
}

// TestCmdModel_Picker_Success drives the full async interactive loop: /model
// (no args) returns immediately with Handled; a background goroutine lists
// models (fake agent), emits a Question card (rpc nil → no-op), and blocks.
// The test reads the requestID from h.Answers, delivers the answer,
// and waits for the goroutine to pin the choice on the router.
func TestCmdModel_Picker_Success(t *testing.T) {
	h, r := newPickerHandlerWithAgent(t, pickerFakeAgent{
		models: []string{"p/a", "p/b", "p/c"},
	})

	res, err := h.cmdModel(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdModel returned error: %v", err)
	}
	if !res.Handled {
		t.Error("async picker should return Handled=true immediately")
	}

	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"p/b"}})

	if got := waitModelSpec(t, r, "chat-1", "p/b", 2*time.Second); got != "p/b" {
		t.Errorf("modelSpec = %q, want p/b", got)
	}
}

// TestCmdModel_Picker_CustomWins verifies a custom-typed value (from the
// input box) overrides the select pick.
func TestCmdModel_Picker_CustomWins(t *testing.T) {
	h, r := newPickerHandlerWithAgent(t, pickerFakeAgent{
		models: []string{"p/a", "p/b"},
	})

	if _, err := h.cmdModel(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdModel: %v", err)
	}

	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"p/a"}, Custom: "manual/x"})

	if got := waitModelSpec(t, r, "chat-1", "manual/x", 2*time.Second); got != "manual/x" {
		t.Errorf("modelSpec = %q, want manual/x (custom overrides choice)", got)
	}
}

// TestCmdAgent_Picker_Success is the agent analogue of the model picker test.
func TestCmdAgent_Picker_Success(t *testing.T) {
	h, r := newPickerHandlerWithAgent(t, pickerFakeAgent{
		agents: []string{"build", "explore", "plan"},
	})

	if _, err := h.cmdAgent(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdAgent: %v", err)
	}

	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"explore"}})

	if got := waitAgentSpec(t, r, "chat-1", "explore", 2*time.Second); got != "explore" {
		t.Errorf("agent = %q, want explore", got)
	}
}

// TestCmdModel_Picker_ListError verifies a failing ListModels leaves no
// dangling pending slot. The error is logged in the background goroutine;
// the assertion is that no slot registers (ListModels fails before
// registerAnswer is reached).
func TestCmdModel_Picker_ListError(t *testing.T) {
	h, _ := newPickerHandlerWithAgent(t, failingListAgent{})

	if _, err := h.cmdModel(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdModel should return nil error (async): %v", err)
	}
	// ListModels fails fast (fake), so the goroutine should exit without ever
	// registering a slot. Poll briefly to confirm.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(h.Answers.PendingIDs()) == 0 {
			return // good: no dangling slot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("expected no pending slot after list error, but one lingered")
}

// TestCmdModel_Picker_EmptyAnswer verifies that an answer with neither a
// choice nor custom text is reported via an error Notice (goroutine path) and
// does not change the binding.
func TestCmdModel_Picker_EmptyAnswer(t *testing.T) {
	h, r := newPickerHandlerWithAgent(t, pickerFakeAgent{models: []string{"p/a"}})

	if _, err := h.cmdModel(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdModel: %v", err)
	}

	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{})

	// The goroutine returns an error from askAndWait (未选择); the binding's
	// ModelSpec must stay empty. Give the goroutine a moment to process.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(h.Answers.PendingIDs()) == 0 {
			break // goroutine consumed the answer and exited
		}
		time.Sleep(5 * time.Millisecond)
	}
	b, _ := r.Lookup("chat-1")
	if b.ModelSpec != "" {
		t.Errorf("modelSpec = %q, want empty after empty answer", b.ModelSpec)
	}
}

// TestDrainAnswers_OnClose verifies Close wakes every blocked picker so the
// process can exit without leaking goroutines.
func TestDrainAnswers_OnClose(t *testing.T) {
	h, _ := newPickerHandlerWithAgent(t, pickerFakeAgent{models: []string{"p/a"}})

	if _, err := h.cmdModel(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdModel: %v", err)
	}
	waitPending(t, h, time.Second) // ensure the picker is blocked on the channel

	// Close in a goroutine: drainAnswers (or appCancel) unblocks askAndWait.
	// Without drainAnswers this would hang until askWaitTimeout (9 min).
	go h.Close()

	// The picker goroutine exits; pendingAnswers drains. Poll for it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.Answers.PendingIDs()) == 0 {
			return // picker unblocked, slot cleared — no leak
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Close did not wake the blocked picker — goroutine leak")
}

// --- helpers ---

func newPickerHandler(t *testing.T) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, nil, nil, HandlerConfig{DefaultDirectory: t.TempDir()}, log.Nop())
	return h, r
}

func newPickerHandlerWithAgent(t *testing.T, agent opencodeAPI) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	// rpc stays nil: emit becomes a no-op so the Question card is "sent" to
	// nowhere. The test reads the requestID straight from h.Answers
	// and delivers the answer itself, exercising the same routing the IPC
	// path would.
	h := NewWithLogger(r, agent, nil, HandlerConfig{DefaultDirectory: t.TempDir()}, log.Nop())
	return h, r
}

// waitPending polls h.Answers until a slot appears, returning its
// requestID. Fails the test if no slot appears within timeout.
func waitPending(t *testing.T, h *Handler, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ids := h.Answers.PendingIDs(); len(ids) > 0 {
			return ids[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no pending answer slot appeared within timeout")
	return ""
}

// waitModelSpec polls the router until the chat's ModelSpec equals want or
// timeout. Used to synchronise on the async picker goroutine's write.
func waitModelSpec(t *testing.T, r *router.Router, chatID, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, ok := r.Lookup(chatID)
		if ok && b.ModelSpec == want {
			return b.ModelSpec
		}
		time.Sleep(2 * time.Millisecond)
	}
	b, _ := r.Lookup(chatID)
	return b.ModelSpec
}

// waitAgentSpec is the agent analogue of waitModelSpec.
func waitAgentSpec(t *testing.T, r *router.Router, chatID, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, ok := r.Lookup(chatID)
		if ok && b.Agent == want {
			return b.Agent
		}
		time.Sleep(2 * time.Millisecond)
	}
	b, _ := r.Lookup(chatID)
	return b.Agent
}
