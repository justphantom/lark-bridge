package claudebridge

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/claude"
	"github.com/hu/lark-bridge/internal/feishufront"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// scriptClaude is a fake claudeAPI that replays a fixed slice of events then
// closes the channel. It lets stream_loop tests assert the emitted Control
// sequence without constructing a real Claude subprocess.
type scriptClaude struct {
	events []claude.Event
}

func (s *scriptClaude) IsReady(context.Context) error { return nil }

func (s *scriptClaude) ListSettings(context.Context) ([]string, error) { return nil, nil }

func (s *scriptClaude) Run(_ context.Context, _ claude.RunOptions) (<-chan claude.Event, error) {
	ch := make(chan claude.Event, len(s.events))
	for i := range s.events {
		ch <- s.events[i]
	}
	close(ch)
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
	client, err := backendrpc.Connect("claude-1", "claude", ts.URL, "")
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
// period to capture late-arriving fire-and-forget controls (async emit
// goroutines may complete after the synchronous terminal control). Returns
// all controls collected, including the terminal one.
func drainUntilTerminal(t *testing.T, reg *feishufront.BackendRegistry) []*protocol.Control {
	t.Helper()
	var controls []*protocol.Control
	for {
		ctrl := drainControl(t, reg)
		controls = append(controls, ctrl)
		if ctrl.Type == protocol.TypeResult || ctrl.Type == protocol.TypeError || ctrl.Type == protocol.TypeNotice {
			// Drain late-arriving async controls.
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
		types[i] = string(c.Type)
	}
	return types
}

// TestHandleEvent_PromptEmitsTerminal verifies a Prompt event drives runPrompt
// to completion and emits a terminal Control. With a closed (empty) stream the
// result is a defensive error, surfaced as TypeError.
func TestHandleEvent_PromptEmitsTerminal(t *testing.T) {
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, closedStreamClaude{}, client, HandlerConfig{
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

// TestHandleEvent_AbortNoOpOnIdleChat verifies an Abort event for a chat with
// no in-flight prompt is handled without error.
func TestHandleEvent_AbortNoOpOnIdleChat(t *testing.T) {
	client, _, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, closedStreamClaude{}, client, HandlerConfig{}, log.Nop())

	ev := &protocol.Event{
		Type:     protocol.TypeAbort,
		PromptID: "msg-1",
		Abort:    &protocol.AbortPayload{ChatID: "c1"},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent abort: %v", err)
	}
}

// TestHandleEvent_PingIsNoOp verifies a Ping event returns nil.
func TestHandleEvent_PingIsNoOp(t *testing.T) {
	client, _, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, closedStreamClaude{}, client, HandlerConfig{}, log.Nop())

	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePing}); err != nil {
		t.Fatalf("HandleEvent ping: %v", err)
	}
}

// TestHandleEvent_UnknownTypeReturnsError verifies an unknown event type is
// rejected.
// TestHandleEvent_SkillPromptBypassesSlashCommand verifies that a prompt with
// Skill=true is treated as a normal prompt even when its text starts with "/".
// Without the flag, "/help" would be dispatched as a local command and emit a
// TypeNotice; with the flag it reaches the (closed) stream and emits TypeError.
func TestHandleEvent_SkillPromptBypassesSlashCommand(t *testing.T) {
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, closedStreamClaude{}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-1",
		Prompt:   &protocol.PromptPayload{ChatID: "c1", Text: "/help", Skill: true},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	ctrl := drainControl(t, reg)
	if ctrl.Type != protocol.TypeError {
		t.Fatalf("expected prompt to hit the stream (TypeError), got %q", ctrl.Type)
	}
}

func TestHandleEvent_UnknownTypeReturnsError(t *testing.T) {
	client, _, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, closedStreamClaude{}, client, HandlerConfig{}, log.Nop())

	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: "bogus"}); err == nil {
		t.Fatal("expected error for unknown event type")
	}
}

// blockingClaude mimics a Claude subprocess whose stdout stays open until the
// run context is cancelled (i.e. the SIGKILL reaches the process group and
// the pipe closes). It lets Close-waits-for-runPrompt be exercised without a
// real subprocess.
type blockingClaude struct{}

func (blockingClaude) IsReady(context.Context) error { return nil }

func (blockingClaude) ListSettings(context.Context) ([]string, error) { return nil, nil }

func (blockingClaude) Run(ctx context.Context, _ claude.RunOptions) (<-chan claude.Event, error) {
	ch := make(chan claude.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// TestClose_WaitsForInFlightPrompt verifies Close does not return before an
// in-flight runPrompt winds down: the goroutine stays in wg until ctx cancel
// propagates and the stream channel closes.
func TestClose_WaitsForInFlightPrompt(t *testing.T) {
	client, _, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, blockingClaude{}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "om_slow",
		Prompt:   &protocol.PromptPayload{ChatID: "c_slow", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Close cancels appCtx → the blocking channel closes → runPrompt returns →
	// wg.Done. This should complete well within shutdownGrace.
	start := time.Now()
	done := make(chan struct{})
	go func() {
		h.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace + 2*time.Second):
		t.Fatal("Close did not return within shutdownGrace")
	}
	if d := time.Since(start); d > shutdownGrace {
		t.Errorf("Close waited %v, should have returned well under shutdownGrace", d)
	}
}
