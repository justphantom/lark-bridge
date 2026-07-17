package miniagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// fakeLLM returns queued responses in order. If a response sets err, Do
// returns it. This drives loop.Run without network.
type fakeLLM struct {
	responses []Response
	errs      []error
	calls     int
	lastReq   Request
}

func (f *fakeLLM) Do(_ context.Context, req Request) (Response, error) {
	f.lastReq = req
	idx := f.calls
	f.calls++
	if idx < len(f.errs) && f.errs[idx] != nil {
		return Response{}, f.errs[idx]
	}
	if idx < len(f.responses) {
		return f.responses[idx], nil
	}
	return Response{}, errors.New("fakeLLM: no more queued responses")
}

// TestRun_TextOnlyReturnsImmediately verifies the P0 happy path: a single
// LLM call returning plain text terminates the loop with that text and
// reflects the usage.
func TestRun_TextOnlyReturnsImmediately(t *testing.T) {
	llm := &fakeLLM{responses: []Response{{
		Text:  "hello world",
		Usage: Usage{InputTokens: 10, OutputTokens: 5},
	}}}
	res, err := Run(context.Background(), llm, LoopConfig{Model: "gpt-4o-mini", System: "be brief"}, "p1", "hi", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "hello world" {
		t.Errorf("Text = %q, want hello world", res.Text)
	}
	if res.Steps != 1 {
		t.Errorf("Steps = %d, want 1", res.Steps)
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Errorf("Usage = %+v, want {10 5}", res.Usage)
	}
	if llm.calls != 1 {
		t.Errorf("llm called %d times, want 1", llm.calls)
	}
	// System + user prompt must reach the LLM.
	if llm.lastReq.System != "be brief" {
		t.Errorf("System = %q, want be brief", llm.lastReq.System)
	}
	if len(llm.lastReq.Messages) != 1 || llm.lastReq.Messages[0].Content != "hi" {
		t.Errorf("Messages = %+v, want one user 'hi'", llm.lastReq.Messages)
	}
}

// TestRun_ToolCallRejectedInP0 verifies that a tool-call response fails the
// turn in P0 (no tools wired) rather than silently looping.
func TestRun_ToolCallRejectedInP0(t *testing.T) {
	llm := &fakeLLM{responses: []Response{{
		Text:      "",
		ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Args: "{}"}},
	}}}
	_, err := Run(context.Background(), llm, LoopConfig{}, "p1", "read x", nil, nil)
	if err == nil {
		t.Fatal("expected error for tool call in P0, got nil")
	}
	if !strings.Contains(err.Error(), "not yet supported") {
		t.Errorf("error = %v, want contains 'not yet supported'", err)
	}
}

// TestRun_LLMErrorPropagates verifies an LLM failure surfaces as the loop's
// error (the handler emits TypeError from this).
func TestRun_LLMErrorPropagates(t *testing.T) {
	llm := &fakeLLM{
		errs: []error{errors.New("upstream 503")},
	}
	_, err := Run(context.Background(), llm, LoopConfig{}, "p1", "hi", nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want contains '503'", err)
	}
}

// TestRun_NilClientErrors verifies the guard against a missing client.
func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "p1", "hi", nil, nil); err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
}

// TestRun_CancelledCtx verifies ctx cancellation between calls aborts the
// loop before the first LLM call.
func TestRun_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	llm := &fakeLLM{responses: []Response{{Text: "x"}}}
	_, err := Run(ctx, llm, LoopConfig{}, "p1", "hi", nil, nil)
	if err == nil {
		t.Fatal("expected error on cancelled ctx, got nil")
	}
}
