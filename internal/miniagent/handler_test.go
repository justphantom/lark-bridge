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
	h := New(llm, LoopConfig{Model: "m"}, rpc, log.Nop(), nil)

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

// TestHandleEvent_EmitsErrorOnLLMFailure verifies an LLM failure surfaces as
// TypeError (not a silent hang on the turn card).
func TestHandleEvent_EmitsErrorOnLLMFailure(t *testing.T) {
	llm := &fakeLLM{errs: []error{errBoom}}
	rpc := &captureSender{}
	h := New(llm, LoopConfig{}, rpc, log.Nop(), nil)

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
	h := New(llm, LoopConfig{}, rpc, log.Nop(), nil)

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
	h := New(&fakeLLM{}, LoopConfig{}, &captureSender{}, log.Nop(), nil)
	if err := h.HandleEvent(context.Background(), &protocol.Event{Type: protocol.TypePing}); err != nil {
		t.Errorf("HandleEvent: %v", err)
	}
}

var errBoom = &simpleErr{"boom"}

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }
