package miniagent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
)

// captureSender records every Control sent. Controls returns a snapshot.
type captureSender struct {
	mu       sync.Mutex
	controls []*protocol.Control
}

func (c *captureSender) SendControl(_ context.Context, ctrl *protocol.Control) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.controls = append(c.controls, ctrl)
	return nil
}

func (c *captureSender) Controls() []*protocol.Control {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*protocol.Control, len(c.controls))
	copy(out, c.controls)
	return out
}

// TestHandleEvent_EmitsResultOnSuccess verifies a prompt flows through the
// handler → loop → TypeResult emit, with the LLM's text reaching the
// frontend.
func TestHandleEvent_EmitsResultOnSuccess(t *testing.T) {
	llm := &fakeLLM{responses: []Response{{Text: "answer", Usage: Usage{InputTokens: 5, OutputTokens: 7}}}}
	rpc := &captureSender{}
	h := New(llm, LoopConfig{Model: "m"}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p1", Prompt: &protocol.PromptPayload{ChatID: "chat-1", Text: "hi"}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// runTurn runs on its own goroutine; poll briefly for the terminal emit.
	deadline := time.After(2 * time.Second)
	for {
		if len(rpc.Controls()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for emit")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	ctl := rpc.Controls()[len(rpc.Controls())-1]
	if ctl.Type != protocol.TypeResult {
		t.Fatalf("Type = %s, want %s", ctl.Type, protocol.TypeResult)
	}
	if ctl.Result == nil || ctl.Result.Text != "answer" {
		t.Errorf("Result = %+v, want text 'answer'", ctl.Result)
	}
	if ctl.ChatID != "chat-1" {
		t.Errorf("ChatID = %q, want chat-1", ctl.ChatID)
	}
}

// TestHandleEvent_EmitsStepsOnResult verifies the turn's LLM call count
// (result.Steps) reaches the frontend Result payload so the card can show
// the round count like the other backends do.
func TestHandleEvent_EmitsStepsOnResult(t *testing.T) {
	tool := &fakeTool{name: "read_file", result: ToolResult{Output: "FILE=hello"}}
	llm := &fakeLLM{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Args: `{"path":"a"}`}}},
		{Text: "done", Usage: Usage{InputTokens: 4, OutputTokens: 5}},
	}}
	rpc := &captureSender{}
	h := New(llm, LoopConfig{Model: "m", Tools: []Tool{tool}}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p1", Prompt: &protocol.PromptPayload{ChatID: "chat-1", Text: "read a"}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	controls := waitControls(t, rpc, 3) // tool_use, tool_result, result
	ctl := controls[len(controls)-1]
	if ctl.Type != protocol.TypeResult {
		t.Fatalf("Type = %s, want %s", ctl.Type, protocol.TypeResult)
	}
	if ctl.Result.Steps != 2 {
		t.Errorf("Steps = %d, want 2 (tool step + text step)", ctl.Result.Steps)
	}
}

// TestHandleEvent_EmitsErrorOnLLMFailure verifies an LLM failure surfaces as
// TypeError (not a silent hang on the turn card).
func TestHandleEvent_EmitsErrorOnLLMFailure(t *testing.T) {
	llm := &fakeLLM{errs: []error{errBoom}}
	rpc := &captureSender{}
	h := New(llm, LoopConfig{}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p2", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "x"}}
	_ = h.HandleEvent(context.Background(), ev)

	deadline := time.After(2 * time.Second)
	for {
		if len(rpc.Controls()) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timeout waiting for emit")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	ctl := rpc.Controls()[0]
	if ctl.Type != protocol.TypeError {
		t.Fatalf("Type = %s, want %s", ctl.Type, protocol.TypeError)
	}
	if ctl.Error == nil || !strings.Contains(ctl.Error.Message, "boom") {
		t.Errorf("Error = %+v, want contains 'boom'", ctl.Error)
	}
}

// TestHandleEvent_EmptyPromptNotices verifies empty text returns a Notice
// synchronously and does not start a turn.
func TestHandleEvent_EmptyPromptNotices(t *testing.T) {
	llm := &fakeLLM{}
	rpc := &captureSender{}
	h := New(llm, LoopConfig{}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "p3", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "   "}}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if llm.calls != 0 {
		t.Errorf("LLM called %d times, want 0 for empty prompt", llm.calls)
	}
	controls := rpc.Controls()
	if len(controls) != 1 || controls[0].Type != protocol.TypeNotice {
		t.Errorf("Controls = %d, want 1 Notice", len(controls))
	}
}

// TestHandleEvent_NonPromptIgnored verifies non-Prompt events are dropped.
func TestHandleEvent_NonPromptIgnored(t *testing.T) {
	h := New(&fakeLLM{}, LoopConfig{}, &captureSender{}, log.Nop(), nil, nil, "", nil, "default", nil)
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePing}); err != nil {
		t.Errorf("HandleEvent: %v", err)
	}
}

var errBoom = &simpleErr{"boom"}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// slowLLM blocks on Do until release is closed, then returns its canned
// response. Used to pin a turn in-flight so a second prompt hits busy.
// release is shared across calls; closing it releases ALL concurrent Do
// callers (do not close twice). Each call is otherwise independent.
type slowLLM struct {
	release chan struct{}
	calls   int
	mu      sync.Mutex
}

func newSlowLLM() *slowLLM {
	return &slowLLM{release: make(chan struct{})}
}

func (s *slowLLM) Do(ctx context.Context, _ Request) (Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	select {
	case <-s.release:
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
	return Response{Text: "slow reply"}, nil
}

// calls returns the current Do call count under the lock so -race is happy
// when the test reads it from a different goroutine than the Do writers.
func (s *slowLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// waitControls polls until at least n Controls are captured or the deadline.
func waitControls(t *testing.T, rpc *captureSender, n int) []*protocol.Control {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if got := rpc.Controls(); len(got) >= n {
			return got
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %d controls, got %d", n, len(rpc.Controls()))
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// TestHandleEvent_BusyRejectsSecond verifies a second prompt for a chat that
// already has an in-flight turn is dropped with a Notice (no second LLM
// call, no concurrent goroutine).
func TestHandleEvent_BusyRejectsSecond(t *testing.T) {
	llm := newSlowLLM()
	rpc := &captureSender{}
	h := New(llm, LoopConfig{Model: "m"}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	ev := func(pid string) *protocol.Event {
		return &protocol.Event{Type: protocol.TypePrompt, PromptID: pid, Prompt: &protocol.PromptPayload{ChatID: "c", Text: "hi"}}
	}
	if err := h.HandleEvent(context.Background(), ev("p1")); err != nil {
		t.Fatalf("first HandleEvent: %v", err)
	}
	// Give the first turn a moment to enter the LLM call (startTurn ran,
	// goroutine scheduled, slowLLM.Do now blocking on release).
	time.Sleep(20 * time.Millisecond)

	if err := h.HandleEvent(context.Background(), ev("p2")); err != nil {
		t.Fatalf("second HandleEvent: %v", err)
	}
	// Second prompt should produce an immediate Notice (busy), without
	// waiting for the slow LLM.
	deadline := time.After(500 * time.Millisecond)
	for {
		if got := rpc.Controls(); len(got) >= 1 && got[0].Type == protocol.TypeNotice {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("busy Notice did not arrive; controls=%d", len(rpc.Controls()))
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	// Only one LLM call in flight (the first turn's); the second never ran.
	if calls := llm.callCount(); calls != 1 {
		t.Errorf("llm calls = %d, want 1 (second prompt must not call LLM)", calls)
	}
	llm.release <- struct{}{}
	h.Close()
}

// TestHandleEvent_DistinctChatsConcurrent verifies two different chats run
// concurrently without blocking each other.
func TestHandleEvent_DistinctChatsConcurrent(t *testing.T) {
	llm1 := newSlowLLM()
	rpc := &captureSender{}
	h := New(llm1, LoopConfig{Model: "m"}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	_ = h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePrompt, PromptID: "p1", Prompt: &protocol.PromptPayload{ChatID: "chat-a", Text: "hi"}})
	_ = h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePrompt, PromptID: "p2", Prompt: &protocol.PromptPayload{ChatID: "chat-b", Text: "hi"}})
	time.Sleep(20 * time.Millisecond)

	if calls := llm1.callCount(); calls != 2 {
		t.Errorf("llm calls = %d, want 2 (both chats should run)", calls)
	}
	close(llm1.release)
	h.Close()
}

// TestHandler_CloseWaitsForInFlight verifies Close blocks until the in-flight
// turn finishes (after its ctx is cancelled) rather than stranding it.
func TestHandler_CloseWaitsForInFlight(t *testing.T) {
	llm := newSlowLLM()
	rpc := &captureSender{}
	h := New(llm, LoopConfig{Model: "m"}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	_ = h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePrompt, PromptID: "p1", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "hi"}})
	time.Sleep(20 * time.Millisecond) // let slowLLM.Do block

	// Close cancels the turn ctx → slowLLM.Do returns ctx.Err() → runTurn
	// emits error → endTouch runs. Close should return within closeGrace.
	start := time.Now()
	h.Close()
	if d := time.Since(start); d > closeGrace+500*time.Millisecond {
		t.Errorf("Close took %s, expected ≤ %s+slack", d, closeGrace)
	}
}

// TestAbort_StopsInFlightTurn verifies /session-abort cancels an in-flight
// turn and the turn emits a "已中止" notice (not a TypeError).
func TestAbort_StopsInFlightTurn(t *testing.T) {
	llm := newSlowLLM()
	rpc := &captureSender{}
	h := New(llm, LoopConfig{Model: "m"}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	_ = h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePrompt, PromptID: "p1", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "hi"}})
	time.Sleep(20 * time.Millisecond) // let slowLLM.Do block on release

	// /session-abort arrives as a prompt (user typed it). HandleEvent must
	// abort the in-flight turn (not be rejected as busy) and reply with a
	// success notice; the aborted turn then emits its own "已中止" notice.
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePrompt, PromptID: "p2", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "/session-abort"}}); err != nil {
		t.Fatalf("abort HandleEvent: %v", err)
	}

	// Expect two notices: the "/session-abort" ack ("已请求中止") and the
	// turn's "已中止". Wait for both, then inspect.
	got := waitControls(t, rpc, 2)
	titles := map[string]bool{}
	for _, c := range got {
		if c.Type == protocol.TypeNotice && c.Notice != nil {
			titles[c.Notice.Title] = true
		}
	}
	if !titles["已请求中止"] {
		t.Errorf("missing '已请求中止' ack notice; got titles %v", titles)
	}
	if !titles["已中止"] {
		t.Errorf("missing '已中止' turn notice; got titles %v", titles)
	}
	// No TypeError should be emitted for an abort.
	for _, c := range rpc.Controls() {
		if c.Type == protocol.TypeError {
			t.Errorf("abort must not emit TypeError; got %v", c.Error)
		}
	}
	h.Close()
}

// TestAbort_TypeAbortEvent verifies a protocol TypeAbort event (not a typed
// command) also cancels the in-flight turn.
func TestAbort_TypeAbortEvent(t *testing.T) {
	llm := newSlowLLM()
	rpc := &captureSender{}
	h := New(llm, LoopConfig{Model: "m"}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	_ = h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePrompt, PromptID: "p1", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "hi"}})
	time.Sleep(20 * time.Millisecond)

	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypeAbort, Abort: &protocol.AbortPayload{ChatID: "c"}}); err != nil {
		t.Fatalf("TypeAbort HandleEvent: %v", err)
	}
	// The aborted turn emits "已中止".
	got := waitControls(t, rpc, 1)
	if got[0].Type != protocol.TypeNotice || got[0].Notice == nil || got[0].Notice.Title != "已中止" {
		t.Errorf("first control = %+v, want 已中止 notice", got[0])
	}
	h.Close()
}

// TestAbort_IdleReply verifies /session-abort on an idle chat (no in-flight
// turn) replies with "无可中止" rather than erroring.
func TestAbort_IdleReply(t *testing.T) {
	rpc := &captureSender{}
	h := New(&fakeLLM{}, LoopConfig{Model: "m"}, rpc, log.Nop(), nil, nil, "", nil, "default", nil)

	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePrompt, PromptID: "p1", Prompt: &protocol.PromptPayload{ChatID: "c", Text: "/session-abort"}}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	got := waitControls(t, rpc, 1)
	if got[0].Type != protocol.TypeNotice || got[0].Notice == nil || got[0].Notice.Title != "无可中止" {
		t.Errorf("control = %+v, want 无可中止 notice", got[0])
	}
}
