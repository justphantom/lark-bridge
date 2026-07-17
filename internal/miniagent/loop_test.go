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
	res, err := Run(context.Background(), llm, LoopConfig{Model: "gpt-4o-mini", System: "be brief"}, "p1", "hi", nil, nil, nil)
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

// fakeTool is a test Tool that returns a canned result and records its calls.
type fakeTool struct {
	name   string
	result ToolResult
	calls  []string
}

func (f *fakeTool) Spec() ToolSpec { return ToolSpec{Name: f.name, Parameters: map[string]any{"type": "object"}} }
func (f *fakeTool) Call(_ context.Context, args string) ToolResult {
	f.calls = append(f.calls, args)
	return f.result
}

// TestRun_ReActToolThenText verifies the full ReAct 2-step path: LLM asks
// for a tool (step 1) → tool executes → result fed back → LLM replies
// with text (step 2) → loop terminates with that text.
func TestRun_ReActToolThenText(t *testing.T) {
	tool := &fakeTool{name: "read_file", result: ToolResult{Output: "FILE=hello"}}
	llm := &fakeLLM{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Args: `{"path":"a"}`}}},
		{Text: "the file says hello", Usage: Usage{InputTokens: 4, OutputTokens: 5}},
	}}
	var signals []Signal
	emit := func(_ string, s Signal) { signals = append(signals, s) }

	res, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}}, "p1", "read a", nil, emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "the file says hello" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2", res.Steps)
	}
	if len(tool.calls) != 1 || tool.calls[0] != `{"path":"a"}` {
		t.Errorf("tool calls = %v, want one call with args", tool.calls)
	}
	// Expect exactly one tool_use and one tool_result signal.
	if len(signals) != 2 {
		t.Fatalf("signals = %d, want 2", len(signals))
	}
	if signals[0].Kind != SignalToolUse || signals[0].Name != "read_file" {
		t.Errorf("signal[0] = %+v, want tool_use/read_file", signals[0])
	}
	if signals[1].Kind != SignalToolResult || signals[1].Output != "FILE=hello" {
		t.Errorf("signal[1] = %+v, want tool_result with output", signals[1])
	}
}

// TestRun_UnknownToolYieldsErrorResult verifies that an unregistered tool
// name produces an IsError tool result and the loop continues (does not
// crash); the LLM's next response terminates.
func TestRun_UnknownToolYieldsErrorResult(t *testing.T) {
	llm := &fakeLLM{responses: []Response{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "no_such", Args: "{}"}}},
		{Text: "ok"},
	}}
	var signals []Signal
	emit := func(_ string, s Signal) { signals = append(signals, s) }
	res, err := Run(context.Background(), llm, LoopConfig{}, "p1", "x", nil, emit, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Steps != 2 {
		t.Errorf("Steps = %d, want 2", res.Steps)
	}
	// The tool_result signal must flag IsError for the unknown tool.
	var tr Signal
	for _, s := range signals {
		if s.Kind == SignalToolResult {
			tr = s
		}
	}
	if tr.Name == "" {
		t.Fatal("no tool_result signal emitted")
	}
	if !tr.IsError {
		t.Errorf("unknown tool result IsError = false, want true")
	}
}

// TestRun_LLMErrorPropagates verifies an LLM failure surfaces as the loop's
// error (the handler emits TypeError from this).
func TestRun_LLMErrorPropagates(t *testing.T) {
	llm := &fakeLLM{
		errs: []error{errors.New("upstream 503")},
	}
	_, err := Run(context.Background(), llm, LoopConfig{}, "p1", "hi", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want contains '503'", err)
	}
}

// TestRun_NilClientErrors verifies the guard against a missing client.
func TestRun_NilClientErrors(t *testing.T) {
	if _, err := Run(context.Background(), nil, LoopConfig{}, "p1", "hi", nil, nil, nil); err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
}

// TestRun_CancelledCtx verifies ctx cancellation between calls aborts the
// loop before the first LLM call.
func TestRun_CancelledCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	llm := &fakeLLM{responses: []Response{{Text: "x"}}}
	_, err := Run(ctx, llm, LoopConfig{}, "p1", "hi", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error on cancelled ctx, got nil")
	}
}
