package claudebridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/claude"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// closedStreamClaude is a fake claudeAPI whose Run returns an already-closed
// event channel. streamRun then falls through to its defensive "no terminal
// event" return path, so runPrompt completes without needing to construct
// claude.Event values (whose fields are unexported).
type closedStreamClaude struct{}

func (closedStreamClaude) ListSettings(context.Context) ([]string, error) { return nil, nil }

func (closedStreamClaude) Run(_ context.Context, _ claude.RunOptions) (<-chan claude.Event, error) {
	ch := make(chan claude.Event)
	close(ch)
	return ch, nil
}

// newTestHandler builds a Handler wired to a closed-stream Claude fake and no
// rpc (runPrompt's emit path tolerates a nil rpc because the closed stream
// yields a defensive error result, which emitTerminal sends — but for these
// context/busy tests the rpc send error is swallowed). Suitable for driving
// runPrompt to completion in tests.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	return NewWithLogger(r, closedStreamClaude{}, nil, HandlerConfig{
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

	promptCtx, mine, ok := h.startPrompt(context.Background(), "chat-1")
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

	_, mine1, ok := h.startPrompt(context.Background(), "chat-busy")
	if !ok {
		t.Fatal("first startPrompt returned ok=false")
	}
	defer mine1.Cancel()

	if _, _, ok2 := h.startPrompt(context.Background(), "chat-busy"); ok2 {
		t.Fatal("second startPrompt for busy chat should return ok=false")
	}
}

// panicClaude is a fake claudeAPI whose Run panics. It locks in the
// runPrompt defer-recover: a panic anywhere in the agent run path must
// be recovered so a single bad turn never crashes the backend process.
type panicClaude struct{}

func (panicClaude) ListSettings(context.Context) ([]string, error) { return nil, nil }
func (panicClaude) Run(context.Context, claude.RunOptions) (<-chan claude.Event, error) {
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
	h := NewWithLogger(r, panicClaude{}, nil, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.New(&log.LevelVar{}, &logBuf, "test"))

	binding, err := h.ensureBinding("chat-panic", "", "", "", "")
	if err != nil {
		t.Fatalf("ensureBinding: %v", err)
	}
	promptCtx, mine, ok := h.startPrompt(context.Background(), "chat-panic")
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

// TestRunPrompt_TimeoutFires verifies that a PromptTimeout > 0 cancels a
// stuck CLI and the terminal notice distinguishes timeout from user cancel
// (R2). blockingClaude never produces events, so the ctx.Err() defensive
// return in streamRun sets isCancelled, and emitTerminal checks
// context.Cause to render "请求超时".
func TestRunPrompt_TimeoutFires(t *testing.T) {
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, blockingClaude{}, client, HandlerConfig{
		StateDir:      t.TempDir(),
		PromptTimeout: 100 * time.Millisecond,
	}, log.Nop())
	r.Bind("c-to", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-to",
		Prompt:   &protocol.PromptPayload{ChatID: "c-to", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	controls := drainUntilTerminal(t, reg)
	for _, c := range controls {
		if c.Type == protocol.TypeNotice {
			if c.Notice.Title != "请求超时" {
				t.Errorf("Notice Title = %q, want 请求超时", c.Notice.Title)
			}
			return
		}
	}
	t.Fatalf("no Notice control received; got %d controls: %v", len(controls), controlTypes(controls))
}

// TestRunPrompt_UserCancelShowsCancelled verifies that a user-initiated
// abort (not a timeout) renders "已取消", not "请求超时" (R2 cause
// distinction).
func TestRunPrompt_UserCancelShowsCancelled(t *testing.T) {
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, blockingClaude{}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c-cancel", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-cancel",
		Prompt:   &protocol.PromptPayload{ChatID: "c-cancel", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Give the prompt a moment to start, then abort.
	time.Sleep(50 * time.Millisecond)
	h.abortChat("c-cancel")

	controls := drainUntilTerminal(t, reg)
	for _, c := range controls {
		if c.Type == protocol.TypeNotice {
			if c.Notice.Title != "已取消" {
				t.Errorf("Notice Title = %q, want 已取消", c.Notice.Title)
			}
			return
		}
	}
	t.Fatalf("no Notice control received; got %d controls: %v", len(controls), controlTypes(controls))
}
