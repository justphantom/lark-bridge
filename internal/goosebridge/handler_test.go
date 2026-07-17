package goosebridge

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/feishufront"
	"github.com/hu/lark-bridge/internal/goose"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// scriptedGoose is a fake gooseAPI that emits a scripted sequence of events,
// then closes the channel. It lets handler tests drive streamRun without a
// real goose subprocess.
type scriptedGoose struct {
	events []goose.Event
}

func (s scriptedGoose) Run(_ context.Context, _ goose.RunOptions) (<-chan goose.Event, error) {
	ch := make(chan goose.Event, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// closedStreamGoose returns an already-closed event channel so streamRun hits
// its defensive "no terminal event" error path.
type closedStreamGoose struct{}

func (closedStreamGoose) Run(_ context.Context, _ goose.RunOptions) (<-chan goose.Event, error) {
	ch := make(chan goose.Event)
	close(ch)
	return ch, nil
}

// blockingGoose mimics a goose subprocess whose stdout stays open until the
// run context is cancelled.
type blockingGoose struct{}

func (blockingGoose) Run(ctx context.Context, _ goose.RunOptions) (<-chan goose.Event, error) {
	ch := make(chan goose.Event)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

// connectTestRPC spins up a real IPCServer + backendrpc.Client pair so the
// Handler emits Controls exactly as in production.
func connectTestRPC(t *testing.T) (*backendrpc.Client, *feishufront.BackendRegistry, func()) {
	t.Helper()
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	client, err := backendrpc.Connect("goose-1", "goose", ts.URL, "")
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

// newTestHandler builds a Handler wired to a fake agent and a real IPC pair.
// The caller drains reg.Controls() to assert on emitted Controls.
func newTestHandler(t *testing.T, api gooseAPI) (*Handler, *feishufront.BackendRegistry, func()) {
	t.Helper()
	r, err := router.New(nil, "", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	rpc, reg, rpcCleanup := connectTestRPC(t)
	dir := t.TempDir()
	h := NewWithLogger(r, api, rpc, HandlerConfig{
		DefaultDirectory: dir,
		StateDir:         dir,
	}, log.Nop())
	cleanup := func() {
		h.Close()
		rpcCleanup()
		r.Close()
	}
	return h, reg, cleanup
}

func promptEvent(chatID, text string) *protocol.Event {
	return &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "p-1",
		Prompt:   &protocol.PromptPayload{ChatID: chatID, Text: text},
	}
}

// TestHandleEvent_PromptEmitsResult: a text + complete stream produces a
// TypeResult carrying the assembled reply.
func TestHandleEvent_PromptEmitsResult(t *testing.T) {
	api := scriptedGoose{events: []goose.Event{
		goose.NewTextEvent("你好"),
		goose.NewTextEvent("，世界"),
		goose.NewCompleteEvent(100, 20),
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	// Bind a directory first so ensureBinding yields a runnable binding.
	h.Router.Bind("chat-1", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), promptEvent("chat-1", "hi")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	var gotResult bool
	deadline := time.After(5 * time.Second)
	for !gotResult {
		select {
		case c := <-reg.Controls():
			if c.Control.Type == protocol.TypeResult {
				gotResult = true
				if c.Control.Result.Text != "你好，世界" {
					t.Errorf("result text = %q, want %q", c.Control.Result.Text, "你好，世界")
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for result control")
		}
	}
}

// TestHandleEvent_ThinkingForwarded: a thinking event is emitted as
// TypeThinking (throttle permitting).
func TestHandleEvent_ThinkingForwarded(t *testing.T) {
	api := scriptedGoose{events: []goose.Event{
		goose.NewThinkingEvent("reasoning..."),
		goose.NewTextEvent("answer"),
		goose.NewCompleteEvent(10, 5),
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-1", "", t.TempDir(), "", "", "")
	_ = h.HandleEvent(context.Background(), promptEvent("chat-1", "hi"))

	var gotThinking, gotResult bool
	deadline := time.After(5 * time.Second)
	for !gotResult {
		select {
		case c := <-reg.Controls():
			switch c.Control.Type {
			case protocol.TypeThinking:
				gotThinking = true
				if c.Control.Thinking.Delta != "reasoning..." {
					t.Errorf("thinking delta = %q, want %q", c.Control.Thinking.Delta, "reasoning...")
				}
			case protocol.TypeResult:
				gotResult = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for result")
		}
	}
	// text/thinking controls are fire-and-forget (emitAsync); drain a brief
	// grace window for the goroutine that may still be in flight.
	grace := time.After(500 * time.Millisecond)
	for !gotThinking {
		select {
		case c := <-reg.Controls():
			if c.Control.Type == protocol.TypeThinking {
				gotThinking = true
			}
		case <-grace:
			t.Error("want a TypeThinking control")
			return
		}
	}
}

// TestHandleEvent_ToolUseFlow: toolRequest + toolResponse + complete emit the
// TypeToolUse / TypeToolResult / TypeResult controls in order.
func TestHandleEvent_ToolUseFlow(t *testing.T) {
	api := scriptedGoose{events: []goose.Event{
		goose.NewToolUseEvent("shell", "tool_1"),
		goose.NewToolResultEvent("shell", "tool_1", "total 0", false),
		goose.NewTextEvent("done"),
		goose.NewCompleteEvent(50, 10),
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-1", "", t.TempDir(), "", "", "")
	_ = h.HandleEvent(context.Background(), promptEvent("chat-1", "ls"))

	var gotToolUse, gotToolResult, gotResult bool
	deadline := time.After(5 * time.Second)
	for !gotResult {
		select {
		case c := <-reg.Controls():
			switch c.Control.Type {
			case protocol.TypeToolUse:
				gotToolUse = true
				if c.Control.ToolUse.Name != "shell" {
					t.Errorf("tool_use name = %q, want shell", c.Control.ToolUse.Name)
				}
			case protocol.TypeToolResult:
				gotToolResult = true
				if c.Control.ToolResult.IsError {
					t.Error("tool_result should not be error")
				}
			case protocol.TypeResult:
				gotResult = true
				if c.Control.Result.Steps != 1 {
					t.Errorf("result steps = %d, want 1", c.Control.Result.Steps)
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for result")
		}
	}
	// tool_use/tool_result are fire-and-forget (emitAsync); drain a brief
	// grace window for goroutines still in flight when result landed.
	grace := time.After(500 * time.Millisecond)
	for !gotToolUse || !gotToolResult {
		select {
		case c := <-reg.Controls():
			switch c.Control.Type {
			case protocol.TypeToolUse:
				gotToolUse = true
			case protocol.TypeToolResult:
				gotToolResult = true
			}
		case <-grace:
			t.Errorf("want tool_use+tool_result, got use=%v result=%v", gotToolUse, gotToolResult)
			return
		}
	}
}

// TestHandleEvent_ClosedStreamEmitsError: a channel closed without a complete
// event surfaces as a TypeError.
func TestHandleEvent_ClosedStreamEmitsError(t *testing.T) {
	api := closedStreamGoose{}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-1", "", t.TempDir(), "", "", "")
	_ = h.HandleEvent(context.Background(), promptEvent("chat-1", "hi"))

	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-reg.Controls():
			if c.Control.Type == protocol.TypeError {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for error control")
		}
	}
}

// TestHandleEvent_EmptyReplyFallback: a complete event with no prior text and
// no tools surfaces the retry-hint reply rather than a blank card.
func TestHandleEvent_EmptyReplyFallback(t *testing.T) {
	api := scriptedGoose{events: []goose.Event{
		goose.NewCompleteEvent(10, 0),
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-1", "", t.TempDir(), "", "", "")
	_ = h.HandleEvent(context.Background(), promptEvent("chat-1", "hi"))

	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-reg.Controls():
			if c.Control.Type == protocol.TypeResult {
				if !strings.Contains(c.Control.Result.Text, "未返回内容") {
					t.Errorf("empty reply want fallback hint, got %q", c.Control.Result.Text)
				}
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for result")
		}
	}
}

// TestHandleEvent_BusyChatRejected: a second prompt for a chat with an
// in-flight prompt is rejected with a "请稍后" notice.
func TestHandleEvent_BusyChatRejected(t *testing.T) {
	api := blockingGoose{}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-1", "", t.TempDir(), "", "", "")
	_ = h.HandleEvent(context.Background(), promptEvent("chat-1", "first"))
	// Second prompt while the first is still blocking.
	_ = h.HandleEvent(context.Background(), promptEvent("chat-1", "second"))

	deadline := time.After(2 * time.Second)
	for {
		select {
		case c := <-reg.Controls():
			if c.Control.Type == protocol.TypeNotice && strings.Contains(c.Control.Notice.Title, "请稍后") {
				return
			}
		case <-deadline:
			t.Fatal("busy chat should be rejected with a 请稍后 notice")
		}
	}
}

// TestHandleEvent_SessionAnchorBackfilled: a complete event on a run that had
// a SessionName writes that anchor back onto the binding (so the next turn
// resumes).
func TestHandleEvent_SessionAnchorBackfilled(t *testing.T) {
	api := scriptedGoose{events: []goose.Event{
		goose.NewTextEvent("ok"),
		goose.NewCompleteEvent(10, 2),
	}}
	h, _, cleanup := newTestHandler(t, api)
	defer cleanup()

	// Pre-bind with a session anchor simulating a resumed turn.
	h.Router.Bind("chat-1", "feishu:chat-1", t.TempDir(), "", "", "")
	_ = h.HandleEvent(context.Background(), promptEvent("chat-1", "hi"))

	// Give the async run a moment to finish + back-fill.
	deadline := time.After(2 * time.Second)
	for {
		b, ok := h.Router.Lookup("chat-1")
		if ok && b.SessionID == "feishu:chat-1" {
			return
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-deadline:
			t.Fatalf("session anchor not back-filled: got %q", b.SessionID)
		}
	}
}
