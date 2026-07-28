package opencodebridge

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencode"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

func TestSummarizeToolInput(t *testing.T) {
	// toolName is the empty string for the generic cases below (the function
	// only routes on exact "todowrite"); todowrite-specific cases set it.
	tests := []struct {
		name     string
		toolName string
		input    string
		want     string
	}{
		{
			name:  "empty returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "empty object returns empty",
			input: "{}",
			want:  "",
		},
		{
			name:  "read extracts filePath",
			input: `{"filePath":"/opt/codes/README.md"}`,
			want:  "/opt/codes/README.md",
		},
		{
			name:  "bash extracts command",
			input: `{"command":"make test","workdir":"/opt/codes"}`,
			want:  "make test",
		},
		{
			name:  "glob extracts pattern",
			input: `{"pattern":"README*","path":"/opt/codes"}`,
			want:  "README*",
		},
		{
			name:  "task extracts description over prompt",
			input: `{"description":"explore repo","prompt":"read all files","subagent_type":"Explore"}`,
			want:  "explore repo",
		},
		{
			// Generic fallback still works for unknown tools carrying a
			// string field — the todowrite special path is gated on toolName
			// exact equality, so a non-todowrite tool with the same input
			// shape must NOT fold to a count.
			name:  "unknown fields with a string fall back to first string value",
			input: `{"records":[{"id":1}],"note":"hi"}`,
			want:  "hi",
		},
		{
			name:  "unknown fields with no string fall back to raw input",
			input: `{"records":[{"id":1}],"count":3}`,
			want:  `{"records":[{"id":1}],"count":3}`,
		},
		{
			// todowrite special path: a todos array folds to a count and
			// the per-item content never leaks into the summary.
			name:     "todowrite folds todos to count",
			toolName: "todowrite",
			input:    `{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"pending"}]}`,
			want:     "清单 1/2",
		},
		{
			name:  "MCP project (snake_case) extracted",
			input: `{"project":"lark-bridge"}`,
			want:  "lark-bridge",
		},
		{
			name:  "MCP repoPath (camelCase) extracted",
			input: `{"repoPath":"/opt/codes/lark-bridge","mode":"full"}`,
			want:  "/opt/codes/lark-bridge",
		},
		{
			name:  "non-json returned as-is",
			input: "not json",
			want:  "not json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgebase.SummarizeToolInput(tc.toolName, tc.input); got != tc.want {
				t.Errorf("bridgebase.SummarizeToolInput(%q, %q) = %q, want %q", tc.toolName, tc.input, got, tc.want)
			}
		})
	}
}

// eventChan buffers events into a closed channel the way a real opencode Run
// would, so streamRun can be driven directly without a subprocess.
func eventChan(events []opencode.Event) <-chan opencode.Event {
	ch := make(chan opencode.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

// parseLines turns NDJSON step lines into an event slice via the exported
// opencode.ParseEvent (opencode.Event fields are unexported).
func parseLines(t *testing.T, lines ...string) []opencode.Event {
	t.Helper()
	var out []opencode.Event
	for _, l := range lines {
		evs, err := opencode.ParseEvent(l)
		if err != nil {
			t.Fatalf("ParseEvent(%q): %v", l, err)
		}
		out = append(out, evs...)
	}
	return out
}

// TestStreamRun_AccumulatesCostAndTokensAcrossSteps verifies that a multi-step
// turn (N tool-calls steps + a terminal stop step) sums every step's tokens
// AND cost into the result. Previously only the terminal step's cost was kept,
// undercounting cost on tool-heavy turns while tokens were already summed.
func TestStreamRun_AccumulatesCostAndTokensAcrossSteps(t *testing.T) {
	const toolStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"tool-calls","tokens":{"total":800,"input":200,"output":80,"cache":{"read":400,"write":0}},"cost":0.01}}`
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":1500,"input":1000,"output":500,"cache":{"read":300,"write":50}},"cost":0.02}}`

	events := parseLines(t, toolStep, toolStep, stopStep)
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "", nil)

	// cost: 0.01 + 0.01 + 0.02 = 0.04 (would be 0.02 if only the terminal step counted).
	if res.costUSD != 0.04 {
		t.Errorf("costUSD = %v, want 0.04", res.costUSD)
	}
	if res.inputTokens != 1400 { // 200 + 200 + 1000
		t.Errorf("inputTokens = %v, want 1400", res.inputTokens)
	}
	if res.outputTokens != 660 { // 80 + 80 + 500
		t.Errorf("outputTokens = %v, want 660", res.outputTokens)
	}
	if res.cacheRead != 1100 { // 400 + 400 + 300
		t.Errorf("cacheRead = %v, want 1100", res.cacheRead)
	}
}

// TestStreamRun_SingleStepCostIsTerminal guards the single-step turn: no
// accumulation, the result cost equals the sole stop step's cost.
func TestStreamRun_SingleStepCostIsTerminal(t *testing.T) {
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":1500,"input":1000,"output":500,"cache":{"read":300,"write":50}},"cost":0.02}}`

	events := parseLines(t, stopStep)
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "", nil)
	if res.costUSD != 0.02 {
		t.Errorf("costUSD = %v, want 0.02", res.costUSD)
	}
}

// TestStreamRun_OnlyTerminalStepTextBecomesReply pins the multi-step reply
// semantics: opencode emits a separate assistant text part per step — the
// step-N text is the model's preamble before a tool call, the step-(N+1) text
// is the final answer after the tool returns. The reply must carry ONLY the
// terminal step's text. Previously the text accumulator was never reset, so the
// step-N preamble was concatenated onto the final answer (observed on a real
// turn where step-1 explained a task delegation and step-2 delivered the report
// — the user got both glued together).
func TestStreamRun_OnlyTerminalStepTextBecomesReply(t *testing.T) {
	const stepStart = `{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}`
	const preamble = `{"type":"text","sessionID":"s1","part":{"type":"text","text":"PREAMBLE_BEFORE_TOOL"}}`
	const toolStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"tool-calls","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`
	const finalAnswer = `{"type":"text","sessionID":"s1","part":{"type":"text","text":"FINAL_ANSWER_AFTER_TOOL"}}`
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, stepStart, preamble, toolStep, stepStart, finalAnswer, stopStep)
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "", nil)
	if res.err != nil {
		t.Fatalf("streamRun: %v", res.err)
	}
	if !strings.Contains(res.reply, "FINAL_ANSWER_AFTER_TOOL") {
		t.Errorf("reply = %q, want contains 'FINAL_ANSWER_AFTER_TOOL'", res.reply)
	}
	if strings.Contains(res.reply, "PREAMBLE_BEFORE_TOOL") {
		t.Errorf("reply leaked non-terminal step text: %q", res.reply)
	}
}

// TestStreamRun_SingleStepTextSurvives verifies the reset does not harm a
// single-step turn: the sole step's text must still become the reply (the
// reset fires once on the only step_start against an empty builder, so nothing
// is lost).
func TestStreamRun_SingleStepTextSurvives(t *testing.T) {
	const stepStart = `{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}`
	const textEv = `{"type":"text","sessionID":"s1","part":{"type":"text","text":"ONLY_ANSWER"}}`
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, stepStart, textEv, stopStep)
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "", nil)
	if res.err != nil {
		t.Fatalf("streamRun: %v", res.err)
	}
	if !strings.Contains(res.reply, "ONLY_ANSWER") {
		t.Errorf("reply = %q, want contains 'ONLY_ANSWER'", res.reply)
	}
}

// TestStreamRun_ThinkingDoesNotPolluteReply verifies the EventThinking case
// fires (no default-branch drop) and the reasoning text stays out of the
// final reply. opencode emits reasoning as a separate part preceding text in
// the same step; without a dedicated case the thinking block would either
// fall through to default (lost) or be appended to the reply (wrong).
func TestStreamRun_ThinkingDoesNotPolluteReply(t *testing.T) {
	const reasoningEv = `{"type":"reasoning","sessionID":"s1","part":{"type":"reasoning","text":"SECRET_REASONING"}}`
	const textEv = `{"type":"text","sessionID":"s1","part":{"type":"text","text":"final answer"}}`
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, reasoningEv, textEv, stopStep)
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "", nil)
	if res.err != nil {
		t.Fatalf("streamRun: %v", res.err)
	}
	if strings.Contains(res.reply, "SECRET_REASONING") {
		t.Errorf("reply leaked reasoning text: %q", res.reply)
	}
	if !strings.Contains(res.reply, "final answer") {
		t.Errorf("reply = %q, want contains 'final answer'", res.reply)
	}
}

// TestStreamRun_SessionIDPropagatedToResult verifies the sessionID captured
// from the first event carrying it lands on the promptResult. This is the
// bridge-side half of the E2 fix: stream_loop also synthesises a
// TypeSessionInit at that moment so the frontend footer can render
// Model + SessionID; the emit itself is a no-op when rpc is nil, but the
// sessionID propagation downstream of the same code path is assertable here.
func TestStreamRun_SessionIDPropagatedToResult(t *testing.T) {
	const stepStart = `{"type":"step_start","sessionID":"ses_test_123","part":{"type":"step-start"}}`
	const stopStep = `{"type":"step_finish","sessionID":"ses_test_123","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, stepStart, stopStep)
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "glm-5", nil)
	if res.sessionID != "ses_test_123" {
		t.Errorf("sessionID = %q, want ses_test_123", res.sessionID)
	}
}

// TestStreamRun_StepStartDoesNotPanicOnEmptyProgress verifies the simplified
// EventStepStart emit (no Description) is well-formed: it ships an empty
// ProgressPayload rather than a banner string, so the dispatcher bumps
// stepCount without overwriting any standing loading/gate banner. The
// observable bridge-side effect is just that streamRun returns cleanly with
// the right step total; the front-end effect (title shows "第 N 轮" without
// a banner row) is verified in renderer/dispatcher tests.
func TestStreamRun_StepStartDoesNotPanicOnEmptyProgress(t *testing.T) {
	const stepStart = `{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}`
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, stepStart, stepStart, stepStart, stopStep)
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "", nil)
	if res.err != nil {
		t.Fatalf("streamRun: %v", res.err)
	}
	if res.steps != 3 {
		t.Errorf("steps = %d, want 3 (one per step_start)", res.steps)
	}
}

// TestStreamRun_TodoWriteEmitsTypeTodoNotToolResult pins the single-send
// behaviour: a completed todowrite tool result is rewritten to a TypeTodo
// control (so the progress card's todo zone renders ✅/⏳/⬜/✘ rows) and
// NO TypeToolResult is emitted for it (so the card is not doubled up by a
// raw-JSON tool row). The todo input shape matches protocol.TodoItem 1:1.
func TestStreamRun_TodoWriteEmitsTypeTodoNotToolResult(t *testing.T) {
	// Real opencode shape: tool_use carries state.status="completed" and the
	// todos array under state.input; part.tool="todowrite".
	const todoLine = `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","id":"prt_1","sessionID":"s1","messageID":"msg_1","callID":"call_1","tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"写测试","status":"in_progress","priority":"high"},{"content":"跑测试","status":"pending"}]},"output":"[\n  {\"content\":\"写测试\"}\n]","title":"1 todos","metadata":{}}}}`
	const stopLine = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, todoLine, stopLine)
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptOpencode{events: events}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c-todo", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-todo",
		Prompt:   &protocol.PromptPayload{ChatID: "c-todo", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	controls := drainUntilTerminal(t, reg)
	var sawTodo bool
	for _, c := range controls {
		if c.Type == protocol.TypeTodo {
			sawTodo = true
			if len(c.Todo.Todos) != 2 {
				t.Errorf("todo items = %d, want 2", len(c.Todo.Todos))
				continue
			}
			if c.Todo.Todos[0].Content != "写测试" || c.Todo.Todos[0].Status != "in_progress" || c.Todo.Todos[0].Priority != "high" {
				t.Errorf("todo[0] = %+v", c.Todo.Todos[0])
			}
			if c.Todo.Todos[1].Content != "跑测试" || c.Todo.Todos[1].Status != "pending" {
				t.Errorf("todo[1] = %+v", c.Todo.Todos[1])
			}
			continue
		}
		if c.Type == protocol.TypeToolResult && c.ToolResult.Name == "todowrite" {
			t.Errorf("todowrite must NOT emit TypeToolResult (single-send to TypeTodo); got %+v", c.ToolResult)
		}
	}
	if !sawTodo {
		t.Fatalf("no TypeTodo control received; got %d controls: %v", len(controls), controlTypes(controls))
	}
}

// TestStreamRun_TodoWriteFailureFallsBackToToolResult locks the fallback: a
// failed todowrite (state.status="error" → EventToolResult.isToolError=true)
// is NOT rewritten to TypeTodo (the list did not update); it ships as a
// normal TypeToolResult so the user sees the failure on the card.
func TestStreamRun_TodoWriteFailureFallsBackToToolResult(t *testing.T) {
	const todoFailLine = `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","id":"prt_1","sessionID":"s1","messageID":"msg_1","callID":"call_1","tool":"todowrite","state":{"status":"error","input":{"todos":[]},"error":"db locked"}}}`
	const stopLine = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, todoFailLine, stopLine)
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptOpencode{events: events}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c-fail", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-fail",
		Prompt:   &protocol.PromptPayload{ChatID: "c-fail", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	controls := drainUntilTerminal(t, reg)
	var sawFailedToolResult bool
	for _, c := range controls {
		if c.Type == protocol.TypeTodo {
			t.Errorf("failed todowrite must NOT emit TypeTodo; got %+v", c.Todo)
		}
		if c.Type == protocol.TypeToolResult && c.ToolResult.Name == "todowrite" {
			sawFailedToolResult = true
			if !c.ToolResult.IsError {
				t.Errorf("ToolResult IsError = false, want true")
			}
			if !strings.Contains(c.ToolResult.Output, "db locked") {
				t.Errorf("ToolResult Output = %q, want contains 'db locked'", c.ToolResult.Output)
			}
		}
	}
	if !sawFailedToolResult {
		t.Fatalf("no TypeToolResult for failed todowrite; got %d controls: %v", len(controls), controlTypes(controls))
	}
}

// TestStreamRun_OtherToolsUnaffectedByTodoRouting guards the side-effect
// boundary: a non-todowrite tool (bash) still emits TypeToolResult exactly as
// before — the todowrite→TypeTodo rewrite is gated on the tool name, not on
// the input shape.
func TestStreamRun_OtherToolsUnaffectedByTodoRouting(t *testing.T) {
	const bashLine = `{"type":"tool_use","sessionID":"s1","part":{"type":"tool","id":"prt_1","sessionID":"s1","messageID":"msg_1","callID":"call_1","tool":"bash","state":{"status":"completed","input":{"command":"ls"},"output":"file.txt","title":"ls","metadata":{"exit":0}}}}`
	const stopLine = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0}}`

	events := parseLines(t, bashLine, stopLine)
	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptOpencode{events: events}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c-bash", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-bash",
		Prompt:   &protocol.PromptPayload{ChatID: "c-bash", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	controls := drainUntilTerminal(t, reg)
	var sawBashResult bool
	for _, c := range controls {
		if c.Type == protocol.TypeTodo {
			t.Errorf("bash tool must NOT emit TypeTodo; got %+v", c.Todo)
		}
		if c.Type == protocol.TypeToolResult && c.ToolResult.Name == "bash" {
			sawBashResult = true
		}
	}
	if !sawBashResult {
		t.Fatalf("no TypeToolResult for bash; got %d controls: %v", len(controls), controlTypes(controls))
	}
}

// TestStreamRun_IdleTimeoutMarked pins the idle-watchdog discrimination:
// when ctx is cancelled with the errIdleTimeout cause (set by runPrompt's
// idle timer), streamRun must return isIdleTimeout=true and isCancelled=
// false, so emitTerminal shows "响应超时" rather than the generic "已取消".
// The channel is closed after cancel to mirror the real pump, which closes
// its event channel once the SIGKILLed subprocess is reaped.
func TestStreamRun_IdleTimeoutMarked(t *testing.T) {
	const stepStart = `{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}`
	events := make(chan opencode.Event, 1)
	events <- parseLines(t, stepStart)[0]

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		cancel(errIdleTimeout)
		close(events)
	}()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(ctx, "c1", "p1", events, "", func() {})
	if !res.isIdleTimeout {
		t.Errorf("isIdleTimeout = false, want true (idle watchdog cause)")
	}
	if res.isCancelled {
		t.Errorf("isCancelled = true, want false; idle must not masquerade as user-cancel")
	}
	if res.err == nil {
		t.Errorf("err = nil, want the cancelled ctx error")
	}
}

// TestStreamRun_UserCancelNotIdle is the inverse: a plain /session-abort
// (context.Canceled cause, NOT errIdleTimeout) must surface as isCancelled,
// never as isIdleTimeout, so the two terminal notices stay distinct.
func TestStreamRun_UserCancelNotIdle(t *testing.T) {
	const stepStart = `{"type":"step_start","sessionID":"s1","part":{"type":"step-start"}}`
	events := make(chan opencode.Event, 1)
	events <- parseLines(t, stepStart)[0]

	ctx, cancel := context.WithCancelCause(context.Background())
	go func() {
		cancel(context.Canceled)
		close(events)
	}()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(ctx, "c1", "p1", events, "", func() {})
	if !res.isCancelled {
		t.Errorf("isCancelled = false, want true (user abort)")
	}
	if res.isIdleTimeout {
		t.Errorf("isIdleTimeout = true, want false; a plain cancel must not look idle")
	}
}
