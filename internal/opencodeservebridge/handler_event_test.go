package opencodeservebridge

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencodeserve"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// closedStreamOpencode is a fake opencodeAPI whose Run returns an already-
// closed event channel. streamRun falls through to its defensive "no
// terminal event" return path, so runPrompt completes without needing to
// construct opencodeserve.Event values (whose fields are unexported).
type closedStreamOpencode struct{}

func (closedStreamOpencode) ListModels(context.Context) ([]string, error) { return nil, nil }

func (closedStreamOpencode) ListAgents(context.Context) ([]string, error) { return nil, nil }

func (closedStreamOpencode) Run(_ context.Context, _ opencodeserve.RunOptions) (<-chan opencodeserve.Event, error) {
	ch := make(chan opencodeserve.Event)
	close(ch)
	return ch, nil
}

// blockingOpencode mimics an opencode subprocess whose stdout stays open
// until the run context is cancelled. It lets timeout/cancel tests exercise
// runPrompt without a real subprocess.
type blockingOpencode struct{}

func (blockingOpencode) ListModels(context.Context) ([]string, error) { return nil, nil }

func (blockingOpencode) ListAgents(context.Context) ([]string, error) { return nil, nil }

func (blockingOpencode) Run(ctx context.Context, _ opencodeserve.RunOptions) (<-chan opencodeserve.Event, error) {
	ch := make(chan opencodeserve.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// connectTestRPC spins up a real IPCServer + backendrpc.Client pair so the
// Handler under test emits Controls exactly as it would in production, and
// the test can read them back from the registry's Controls() channel.
func connectTestRPC(t *testing.T) (*backendrpc.Client, *feishufront.BackendRegistry, func()) {
	t.Helper()
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	client, err := backendrpc.Connect("opencode-1", "opencode", ts.URL, "")
	if err != nil {
		ts.Close()
		t.Fatalf("connect: %v", err)
	}
	cleanup := func() {
		client.Close()
		ts.Close()
	}
	return client, reg, cleanup
}

// drainControl reads one RoutedControl from reg within a 2s timeout.
func drainControl(t *testing.T, reg *feishufront.BackendRegistry) *protocol.Control {
	t.Helper()
	select {
	case rc := <-reg.Controls():
		return rc.Control
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for control")
		return nil
	}
}

// drainUntilTerminal drains controls until a terminal type (Result, Error, or
// Notice from emitTerminal) arrives, then continues draining for a short grace
// period to capture late-arriving fire-and-forget controls. Returns all
// controls collected, including the terminal one.
func drainUntilTerminal(t *testing.T, reg *feishufront.BackendRegistry) []*protocol.Control {
	t.Helper()
	var controls []*protocol.Control
	for {
		ctrl := drainControl(t, reg)
		controls = append(controls, ctrl)
		if ctrl.Type == protocol.TypeResult || ctrl.Type == protocol.TypeError || ctrl.Type == protocol.TypeNotice {
			for {
				select {
				case rc := <-reg.Controls():
					controls = append(controls, rc.Control)
				case <-time.After(200 * time.Millisecond):
					return controls
				}
			}
		}
	}
}

// controlTypes returns a slice of control type strings for debug output.
func controlTypes(controls []*protocol.Control) []string {
	types := make([]string, len(controls))
	for i, c := range controls {
		types[i] = c.Type
	}
	return types
}

// TestHandleEvent_PromptEmitsTerminal verifies a Prompt event drives runPrompt
// to completion and emits a terminal Control. With a closed (empty) stream the
// result is a defensive error, surfaced as TypeError.
func TestHandleEvent_PromptEmitsTerminal(t *testing.T) {
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-1",
		Prompt:   &protocol.PromptPayload{ChatID: "c1", Text: "hi"},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	ctrl := drainControl(t, reg)
	if ctrl.Type != protocol.TypeError {
		t.Fatalf("expected terminal error control (empty stream), got %q", ctrl.Type)
	}
}

// TestRunPrompt_TimeoutFires verifies that a PromptTimeout > 0 cancels a
// stuck CLI and the terminal notice distinguishes timeout from user cancel
// (R2). blockingOpencode never produces events, so the ctx.Err() defensive
// return in streamRun sets isCancelled, and emitTerminal checks
// context.Cause to render "请求超时".
// TestHandleEvent_SkillPromptBypassesSlashCommand verifies that a prompt with
// Skill=true is treated as a normal prompt even when its text starts with "/".
// Without the flag, "/session-abort" would be dispatched as a local command and
// emit a TypeNotice; with the flag it reaches the (closed) stream and emits
// TypeError.
func TestHandleEvent_SkillPromptBypassesSlashCommand(t *testing.T) {
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-1",
		Prompt:   &protocol.PromptPayload{ChatID: "c1", Text: "/session-abort", Skill: true},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	ctrl := drainControl(t, reg)
	if ctrl.Type != protocol.TypeError {
		t.Fatalf("expected prompt to hit the stream (TypeError), got %q", ctrl.Type)
	}
}

func TestRunPrompt_TimeoutFires(t *testing.T) {
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, blockingOpencode{}, client, HandlerConfig{
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
	h := NewWithLogger(r, blockingOpencode{}, client, HandlerConfig{
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
	h.AbortChat("c-cancel")

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
