package opencodeservebridge

import (
	"context"
	"strings"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// newTestHandler builds a Handler wired to a closed-stream opencode fake and
// no rpc. runPrompt's defensive "no terminal event" path completes without
// needing real opencodeserve.Event values. Suitable for driving runPrompt to
// completion in tests.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	return NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
}

// TestRunPromptCancelsContext locks in the leak fix: startPrompt derives the
// prompt context from appCtx via context.WithCancel, so runPrompt MUST call
// mine.Cancel() on exit. Without it the context (and the goroutine spawned by
// context.propagateCancel) would survive until appCtx cancels at process
// shutdown — one leaked goroutine per completed prompt.
func TestRunPromptCancelsContext(t *testing.T) {
	h := newTestHandler(t)

	binding, err := h.ensureBinding("chat-1", "", "", "", "")
	if err != nil {
		t.Fatalf("ensureBinding: %v", err)
	}

	promptCtx, mine, ok := h.StartPrompt(context.Background(), "chat-1")
	if !ok {
		t.Fatal("startPrompt returned ok=false")
	}

	done := make(chan struct{})
	go func() {
		h.runPrompt(promptCtx, "chat-1", binding, "hi", "msg-1", mine)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPrompt did not return within timeout")
	}

	if promptCtx.Err() == nil {
		t.Fatal("prompt context not cancelled after runPrompt returned (goroutine leak)")
	}
}

// TestStartPromptBusy ensures the busy-then-drop gate rejects a second
// in-flight prompt for the same chat and leaves the running slot intact.
func TestStartPromptBusy(t *testing.T) {
	h := newTestHandler(t)

	_, mine1, ok := h.StartPrompt(context.Background(), "chat-busy")
	if !ok {
		t.Fatal("first startPrompt returned ok=false")
	}
	defer mine1.Cancel()

	if _, _, ok2 := h.StartPrompt(context.Background(), "chat-busy"); ok2 {
		t.Fatal("second startPrompt for busy chat should return ok=false")
	}
}

// panicOpencode is a fake opencodeAPI whose Run panics. It locks in the
// runPrompt defer-recover: a panic anywhere in the agent run path must
// be recovered so a single bad turn never crashes the backend process.
type panicOpencode struct{}

func (panicOpencode) ListModels(context.Context) ([]string, error) { return nil, nil }
func (panicOpencode) ListAgents(context.Context) ([]string, error) { return nil, nil }
func (panicOpencode) AbortSession(context.Context, string) error   { return nil }
func (panicOpencode) ListSessions(context.Context, string) ([]oc.SessionInfo, error) {
	return nil, nil
}
func (panicOpencode) SessionStatuses(context.Context) (map[string]oc.SessionStatus, error) {
	return map[string]oc.SessionStatus{}, nil
}
func (panicOpencode) DeleteSessionIfIdle(context.Context, string) error { return nil }
func (panicOpencode) ReplyPermission(context.Context, string, string, string, string) error {
	return nil
}
func (panicOpencode) ReplyQuestion(context.Context, string, string, *oc.QuestionReply) error {
	return nil
}
func (panicOpencode) RejectQuestion(context.Context, string, string) error { return nil }
func (panicOpencode) Run(context.Context, oc.RunOptions) (<-chan oc.HighEvent, error) {
	panic("simulated agent panic")
}

// TestRunPromptRecoversPanic verifies runPrompt's defer-recover contains a
// panicking agent run: the goroutine returns normally (no panic propagates
// to the caller) and the panic is logged. rpc is nil so the post-recover
// emit is a no-op; the point is that the process survives.
func TestRunPromptRecoversPanic(t *testing.T) {
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	var logBuf strings.Builder
	h := NewWithLogger(r, panicOpencode{}, nil, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.New(&log.LevelVar{}, &logBuf, "test"))

	binding, err := h.ensureBinding("chat-panic", "", "", "", "")
	if err != nil {
		t.Fatalf("ensureBinding: %v", err)
	}
	promptCtx, mine, ok := h.StartPrompt(context.Background(), "chat-panic")
	if !ok {
		t.Fatal("startPrompt returned ok=false")
	}

	done := make(chan struct{})
	go func() {
		// A panic here (recover not working) would crash the test process.
		h.runPrompt(promptCtx, "chat-panic", binding, "hi", "msg-panic", mine)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runPrompt did not return within timeout (recover failed?)")
	}

	if !strings.Contains(logBuf.String(), "panic in runPrompt") {
		t.Errorf("expected panic logged, got:\n%s", logBuf.String())
	}
}

// TestEmitTerminal_RescuedSkipsDefault pins the OnIdle-rescue dedup branch:
// when promptResult.rescued is true (the watchdog goroutine already emitted
// TypeResult via handleRescue), emitTerminal must NOT take its default
// branch — otherwise the user sees a duplicate result card for one turn.
// The reverse assertion (rescued=false with a reply does emit TypeResult)
// guards against the branch silently atrophying into an unconditional skip.
//
// emitTerminal uses synchronous emitLogged (not EmitAsync), so by the time
// it returns the IPC round-trip is complete and the captured control is
// observable without a wait.
func TestEmitTerminal_RescuedSkipsDefault(t *testing.T) {
	h, _, captured := newWireHandler(t, closedStreamOpencode{})

	// rescued=true: rescue path already produced the card; emitTerminal
	// must skip its default branch and emit nothing.
	h.emitTerminal(context.Background(), "chat-rescued", "msg-rescued", promptResult{
		rescued:   true,
		sessionID: "sess-rescued",
	})
	if ctrl := captured.find(func(c *protocol.Control) bool {
		return c.Type == protocol.TypeResult && c.ChatID == "chat-rescued"
	}); ctrl != nil {
		t.Errorf("emitTerminal emitted TypeResult despite rescued=true: %+v", ctrl)
	}

	// Reverse: rescued=false + reply → default branch fires TypeResult.
	h.emitTerminal(context.Background(), "chat-normal", "msg-normal", promptResult{
		rescued:   false,
		reply:     "answer",
		sessionID: "sess-normal",
		model:     "p/m",
	})
	ctrl := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeResult && c.ChatID == "chat-normal"
	}, 2*time.Second)
	if ctrl == nil {
		t.Fatal("default branch did not emit TypeResult for rescued=false")
	}
	if ctrl.Result == nil {
		t.Fatal("Result payload nil")
	}
	if ctrl.Result.Text != "answer" {
		t.Errorf("Result.Text = %q, want 'answer'", ctrl.Result.Text)
	}
}
