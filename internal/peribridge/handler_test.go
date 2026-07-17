package peribridge

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/feishufront"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/peri"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// scriptedPeri is a fake periAPI that emits a scripted sequence of events,
// then closes the channel. It lets handler tests drive streamRun without a
// real peri subprocess.
type scriptedPeri struct {
	events []peri.Event
}

func (s scriptedPeri) Run(_ context.Context, _ peri.RunOptions) (<-chan peri.Event, error) {
	ch := make(chan peri.Event, len(s.events))
	for _, ev := range s.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// closedStreamPeri returns an already-closed event channel so streamRun hits
// its defensive "no terminal event" error path.
type closedStreamPeri struct{}

func (closedStreamPeri) Run(_ context.Context, _ peri.RunOptions) (<-chan peri.Event, error) {
	ch := make(chan peri.Event)
	close(ch)
	return ch, nil
}

// blockingPeri mimics a peri subprocess whose stdout stays open until the run
// context is cancelled.
type blockingPeri struct{}

func (blockingPeri) Run(ctx context.Context, _ peri.RunOptions) (<-chan peri.Event, error) {
	ch := make(chan peri.Event)
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
	client, err := backendrpc.Connect("peri-1", "peri", ts.URL, "")
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

// drainControl reads one RoutedControl from reg within a timeout.
func drainControl(t *testing.T, reg *feishufront.BackendRegistry, timeout time.Duration) *protocol.Control {
	t.Helper()
	select {
	case rc := <-reg.Controls():
		return rc.Control
	case <-time.After(timeout):
		t.Fatalf("drainControl: timed out after %s", timeout)
		return nil
	}
}

// newTestHandler builds a Handler wired to a fake agent and a real IPC pair.
// The caller drains reg.Controls() to assert on emitted Controls.
func newTestHandler(t *testing.T, api periAPI) (*Handler, *feishufront.BackendRegistry, func()) {
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

// TestHandleEvent_PromptEmitsResult drives a text+result script through a
// Prompt event and asserts a TypeResult control lands with the accumulated
// reply. This is the core peri-back contract: Event in → Control out.
func TestHandleEvent_PromptEmitsResult(t *testing.T) {
	api := scriptedPeri{events: []peri.Event{
		peri.NewTextEvent("你好"),
		peri.NewTextEvent("，世界"),
		peri.NewResultEvent("你好，世界"),
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	// Bind a directory first so ensureBinding yields a runnable binding.
	h.Router.Bind("chat-1", "", t.TempDir(), "", "", "")

	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "p-1",
		Prompt: &protocol.PromptPayload{
			ChatID: "chat-1",
			Text:   "hi",
		},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Expect at least one text delta then the terminal result. Text/tool
	// controls are emitted async (fire-and-forget) while the result is emitted
	// synchronously, so the result may land first; collect types and drain a
	// short grace window after the result arrives.
	var gotText, gotResult bool
	deadline := time.After(5 * time.Second)
	for !gotResult {
		select {
		case c := <-reg.Controls():
			switch c.Control.Type {
			case protocol.TypeText:
				gotText = true
			case protocol.TypeResult:
				gotResult = true
				if c.Control.Result.Text != "你好，世界" {
					t.Errorf("result text = %q, want %q", c.Control.Result.Text, "你好，世界")
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for result control")
		}
	}
	// Grace period so async text deltas still in flight can land.
	if !gotText {
		select {
		case c := <-reg.Controls():
			if c.Control.Type == protocol.TypeText {
				gotText = true
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !gotText {
		t.Error("expected at least one text control")
	}
}

// TestHandleEvent_AbortCancelsInFlight verifies a TypeAbort event cancels a
// blocking run, producing a cancellation notice rather than a hang.
func TestHandleEvent_AbortCancelsInFlight(t *testing.T) {
	h, reg, cleanup := newTestHandler(t, blockingPeri{})
	defer cleanup()

	h.Router.Bind("chat-2", "", t.TempDir(), "", "", "")

	promptEv := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "p-2",
		Prompt:   &protocol.PromptPayload{ChatID: "chat-2", Text: "long task"},
	}
	if err := h.HandleEvent(context.Background(), promptEv); err != nil {
		t.Fatalf("HandleEvent prompt: %v", err)
	}

	// Give the run a moment to register in cancelByChat.
	time.Sleep(100 * time.Millisecond)

	abortEv := &protocol.Event{
		Type:     protocol.TypeAbort,
		PromptID: "p-2",
		Abort:    &protocol.AbortPayload{ChatID: "chat-2"},
	}
	if err := h.HandleEvent(context.Background(), abortEv); err != nil {
		t.Fatalf("HandleEvent abort: %v", err)
	}

	// The cancelled run should emit a notice (info level, "已取消").
	select {
	case c := <-reg.Controls():
		if c.Control.Type != protocol.TypeNotice {
			t.Errorf("control type = %q, want %q", c.Control.Type, protocol.TypeNotice)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for cancellation notice")
	}
}

// TestHandleEvent_ClosedStreamEmitsError verifies the defensive "stream closed
// without terminal event" path surfaces as a TypeError control.
func TestHandleEvent_ClosedStreamEmitsError(t *testing.T) {
	h, reg, cleanup := newTestHandler(t, closedStreamPeri{})
	defer cleanup()

	h.Router.Bind("chat-3", "", t.TempDir(), "", "", "")

	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "p-3",
		Prompt:   &protocol.PromptPayload{ChatID: "chat-3", Text: "hi"},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case c := <-reg.Controls():
		if c.Control.Type != protocol.TypeError {
			t.Errorf("control type = %q, want %q", c.Control.Type, protocol.TypeError)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for error control")
	}
}

// TestHandleEvent_BusyChatRejected verifies the busy-then-drop gate: a second
// prompt for a chat with an in-flight run is rejected with a notice.
func TestHandleEvent_BusyChatRejected(t *testing.T) {
	h, reg, cleanup := newTestHandler(t, blockingPeri{})
	defer cleanup()

	h.Router.Bind("chat-4", "", t.TempDir(), "", "", "")

	p1 := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p-4a",
		Prompt: &protocol.PromptPayload{ChatID: "chat-4", Text: "first"}}
	if err := h.HandleEvent(context.Background(), p1); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	time.Sleep(100 * time.Millisecond) // let it register as busy

	p2 := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p-4b",
		Prompt: &protocol.PromptPayload{ChatID: "chat-4", Text: "second"}}
	if err := h.HandleEvent(context.Background(), p2); err != nil {
		t.Fatalf("second prompt: %v", err)
	}

	// Drain until we see the "请稍后" busy notice. The first run is blocking,
	// so its controls have not landed yet; the busy notice is emitted inline.
	var sawBusy bool
	deadline := time.After(2 * time.Second)
	for !sawBusy {
		select {
		case c := <-reg.Controls():
			if c.Control.Type == protocol.TypeNotice &&
				c.Control.Notice != nil && c.Control.Notice.Title == "请稍后" {
				sawBusy = true
			}
		case <-deadline:
			t.Fatal("did not see busy-reject notice")
		}
	}
}

// TestHandleEvent_EmptyReplyFallback verifies the bridge surfaces a friendly
// hint instead of a blank card when peri returns no text and no tool call
// (the model-empty-reply case). peri print mode ends with no terminal result
// line; without the fallback the user sees nothing.
func TestHandleEvent_EmptyReplyFallback(t *testing.T) {
	api := scriptedPeri{events: []peri.Event{
		peri.NewResultEvent(""), // empty reply, no prior text/tool
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-empty", "", t.TempDir(), "", "", "")
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p-empty",
		Prompt: &protocol.PromptPayload{ChatID: "chat-empty", Text: "hi"}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	select {
	case c := <-reg.Controls():
		if c.Control.Type != protocol.TypeResult {
			t.Fatalf("control type = %q, want result", c.Control.Type)
		}
		if !strings.Contains(c.Control.Result.Text, "未返回内容") {
			t.Errorf("empty reply fallback = %q, want fallback hint", c.Control.Result.Text)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for result control")
	}
}

// TestHandleEvent_EmptyReplyWithToolsNoFallback verifies the fallback does NOT
// trigger when tools ran (their results are on the progress card; an absent
// final summary is not an error). The result text may be empty in that case.
func TestHandleEvent_EmptyReplyWithToolsNoFallback(t *testing.T) {
	api := scriptedPeri{events: []peri.Event{
		peri.NewToolUseEvent("Bash", "t-1"),
		peri.NewToolResultEvent("Bash", "t-1", "done", false),
		peri.NewResultEvent(""), // tools ran, but no final summary text
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-t", "", t.TempDir(), "", "", "")
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p-t",
		Prompt: &protocol.PromptPayload{ChatID: "chat-t", Text: "run it"}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Tool controls are emitted async (fire-and-forget) while the result is
	// emitted synchronously, so either may arrive first on the loopback IPC;
	// drain until the terminal result lands.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c := <-reg.Controls():
			if c.Control.Type != protocol.TypeResult {
				continue // tool_use/tool_result from the async path
			}
			if strings.Contains(c.Control.Result.Text, "未返回内容") {
				t.Errorf("fallback wrongly applied when tools ran: %q", c.Control.Result.Text)
			}
			return
		case <-deadline:
			t.Fatal("timed out waiting for result control")
		}
	}
}

// TestHandleEvent_ToolUseFlow verifies tool_use + tool_result events map to
// the corresponding controls, then a result lands.
func TestHandleEvent_ToolUseFlow(t *testing.T) {
	api := scriptedPeri{events: []peri.Event{
		peri.NewToolUseEvent("Read", "t-1"),
		peri.NewToolResultEvent("Read", "t-1", "file body", false),
		peri.NewTextEvent("done"),
		peri.NewResultEvent("done"),
	}}
	h, reg, cleanup := newTestHandler(t, api)
	defer cleanup()

	h.Router.Bind("chat-5", "", t.TempDir(), "", "", "")

	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p-5",
		Prompt: &protocol.PromptPayload{ChatID: "chat-5", Text: "read it"}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	var gotToolUse, gotToolResult, gotResult bool
	deadline := time.After(5 * time.Second)
	for !gotResult {
		select {
		case c := <-reg.Controls():
			switch c.Control.Type {
			case protocol.TypeToolUse:
				gotToolUse = true
				if c.Control.ToolUse.Name != "Read" {
					t.Errorf("tool_use name = %q", c.Control.ToolUse.Name)
				}
			case protocol.TypeToolResult:
				gotToolResult = true
				if c.Control.ToolResult.Output != "file body" {
					t.Errorf("tool_result output = %q", c.Control.ToolResult.Output)
				}
			case protocol.TypeResult:
				gotResult = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for result")
		}
	}
	// Grace period for async tool controls still in flight.
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
			if !gotToolUse {
				t.Error("expected a tool_use control")
			}
			if !gotToolResult {
				t.Error("expected a tool_result control")
			}
			return
		}
	}
}
