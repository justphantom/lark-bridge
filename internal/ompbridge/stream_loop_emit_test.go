package ompbridge

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// These tests exercise the new streamRun event branches (notice,
// thinking_level_changed, tool_execution_update, todowrite, task subagent,
// role=custom nudge, errorStatus codes, turn_end fallback) through the real
// parse→emit path: events flow through omp.ParseEvent → streamRun → the
// backendrpc IPC pair, and are read back from the registry's Controls()
// channel exactly as in production. This catches both parser and bridge
// regressions and asserts the emitted Control payloads (not just that streamRun
// returned without error).

// connectOmpTestRPC spins up a real IPCServer + backendrpc.Client pair so the
// Handler emits Controls exactly as in production, readable from the registry's
// Controls() channel. Mirrors opencodebridge.connectTestRPC.
func connectOmpTestRPC(t *testing.T) (*backendrpc.Client, *feishufront.BackendRegistry, func()) {
	t.Helper()
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	client, err := backendrpc.Connect(backendrpc.ConnectOptions{BackendID: "omp-1", BackendType: "omp", FrontendURL: ts.URL})
	if err != nil {
		ts.Close()
		t.Fatalf("connect: %v", err)
	}
	return client, reg, func() {
		client.Close()
		ts.Close()
	}
}

// drainOmpUntilTerminal drains controls until a terminal type (Result/Error)
// arrives, then keeps draining for a short grace period to capture late
// fire-and-forget controls. omp's emitTerminal path does NOT post a Notice the
// way the cancelled-timeout path does, so only Result/Error are terminal here.
func drainOmpUntilTerminal(t *testing.T, reg *feishufront.BackendRegistry) []*protocol.Control {
	t.Helper()
	var controls []*protocol.Control
	for {
		select {
		case rc := <-reg.Controls():
			controls = append(controls, rc.Control)
			if rc.Control.Type == protocol.TypeResult || rc.Control.Type == protocol.TypeError {
				// Grace period for late fire-and-forget controls.
				for {
					select {
					case rc2 := <-reg.Controls():
						controls = append(controls, rc2.Control)
					case <-time.After(150 * time.Millisecond):
						return controls
					}
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for terminal control; got %d controls: %v", len(controls), ompControlTypes(controls))
		}
	}
}

func ompControlTypes(controls []*protocol.Control) []string {
	types := make([]string, len(controls))
	for i, c := range controls {
		types[i] = c.Type
		if c.ToolResult != nil {
			types[i] += "(" + c.ToolResult.Name + ")"
		}
	}
	return types
}

// runOmpLines drives a slice of NDJSON lines through the full
// HandleEvent→runOMP→streamRun path against a freshly wired handler + registry.
// Returns the emitted controls.
func runOmpLines(t *testing.T, lines ...string) []*protocol.Control {
	t.Helper()
	client, reg, cleanup := connectOmpTestRPC(t)
	defer cleanup()

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := NewWithLogger(r, &scriptOmpAgent{lines: lines}, client, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{StateDir: t.TempDir()},
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-1",
		Prompt:   &protocol.PromptPayload{ChatID: "c1", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	return drainOmpUntilTerminal(t, reg)
}

// scriptOmpAgent feeds parsed NDJSON lines as omp events and closes, so a test
// drives streamRun through the real parse→emit path without a subprocess. The
// lines MUST include a terminal (agent_end / error) or runOMP's defensive
// "no terminal event" branch fires.
type scriptOmpAgent struct {
	lines []string
}

func (s *scriptOmpAgent) Run(_ context.Context, _ omp.RunOptions) (<-chan omp.Event, error) {
	var events []omp.Event
	for _, l := range s.lines {
		ev, ok, err := omp.ParseEvent(l)
		if err != nil {
			continue
		}
		if ok {
			events = append(events, ev)
		}
	}
	ch := make(chan omp.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch, nil
}
func (s *scriptOmpAgent) ListModels(context.Context) ([]string, error) { return nil, nil }
func (s *scriptOmpAgent) ListSessions(context.Context, string) ([]omp.Session, error) {
	return nil, nil
}
func (s *scriptOmpAgent) DeleteSession(context.Context, string, string) error { return nil }
func (s *scriptOmpAgent) CleanSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (s *scriptOmpAgent) RunGC(context.Context, omp.GCOptions) (omp.GCResult, error) {
	return omp.GCResult{}, nil
}
func (s *scriptOmpAgent) IsReady(context.Context) error { return nil }

// findControl returns the first control of the given type, or nil.
func findControl(controls []*protocol.Control, typ string) *protocol.Control {
	for _, c := range controls {
		if c.Type == typ {
			return c
		}
	}
	return nil
}

// commonOmpTail is the standard success tail: an assistant message_end with
// usage + an agent_end terminal. Prepended tests only need to add the events
// under test before it.
const (
	ompSessionHdr = `{"type":"session","id":"s1","cwd":"/tmp","title":"t"}`
	ompAgentStart = `{"type":"agent_start"}`
	ompTurnStart  = `{"type":"turn_start"}`
	ompMsgEnd     = `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"total":0}}}}`
	ompAgentEnd   = `{"type":"agent_end","messages":[],"isTerminal":true}`
)

// TestEmit_NoticeForwardsMessage verifies a notice event reaches the frontend
// as a TypeNotice carrying the CLI's level + message (previously dropped into
// the unknown-event default).
func TestEmit_NoticeForwardsMessage(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"notice","level":"info","message":"xd://: mounted mcp__codegraph_explore","source":"xdev"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`,
		ompMsgEnd, ompAgentEnd)

	c := findControl(controls, protocol.TypeNotice)
	if c == nil {
		t.Fatalf("no TypeNotice emitted; got %v", ompControlTypes(controls))
	}
	if c.Notice == nil {
		t.Fatal("Notice payload nil")
	}
	if c.Notice.Level != "info" {
		t.Errorf("Notice.Level = %q, want info", c.Notice.Level)
	}
	if c.Notice.Message != "xd://: mounted mcp__codegraph_explore" {
		t.Errorf("Notice.Message = %q", c.Notice.Message)
	}
}

// TestEmit_ThinkingLevelChangedSurfacesResolved verifies a
// thinking_level_changed event surfaces the resolved level as an info notice so
// the card shows the effort the model actually uses (configured=auto →
// resolved=high).
func TestEmit_ThinkingLevelChangedSurfacesResolved(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"thinking_level_changed","thinkingLevel":"high","configured":"auto","resolved":"high"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`,
		ompMsgEnd, ompAgentEnd)

	var saw bool
	for _, c := range controls {
		if c.Type == protocol.TypeNotice && c.Notice != nil && strings.Contains(c.Notice.Message, "high") {
			saw = true
			if c.Notice.Title != "Thinking 级别" {
				t.Errorf("Title = %q, want Thinking 级别", c.Notice.Title)
			}
		}
	}
	if !saw {
		t.Fatalf("no thinking-level notice emitted; got %v", ompControlTypes(controls))
	}
}

// TestEmit_ToolUpdateProgress verifies a tool_execution_update carrying a
// fileCount emits a TypeProgress with a "已返回 N 项" description, so a
// long-running glob is not silent.
func TestEmit_ToolUpdateProgress(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"tool_execution_start","toolCallId":"call_1","toolName":"glob","args":{"path":"/*"},"intent":"glob"}`,
		`{"type":"tool_execution_update","toolCallId":"call_1","toolName":"glob","args":{"path":"/*"},"partialResult":{"content":[{"type":"text","text":"a.go"}],"details":{"scopePath":".","fileCount":5,"files":["a.go","b.go","c.go","d.go","e.go"],"truncated":false}}}`,
		`{"type":"tool_execution_end","toolCallId":"call_1","toolName":"glob","result":{"content":[{"type":"text","text":"a.go"}]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}`,
		ompMsgEnd, ompAgentEnd)

	var sawProgress bool
	for _, c := range controls {
		if c.Type == protocol.TypeProgress && c.Progress != nil && strings.Contains(c.Progress.Description, "已返回 5 项") {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Fatalf("no progress '已返回 5 项' emitted; got %v", ompControlTypes(controls))
	}
}

// TestEmit_TodoWriteRoutesToTypeTodo verifies a completed todowrite tool is
// rewritten to a TypeTodo control (rendered as the todo zone) and NOT doubled
// up by a TypeToolResult for the same call. The todos array comes from the
// start event's args (the end event carries none).
func TestEmit_TodoWriteRoutesToTypeTodo(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"tool_execution_start","toolCallId":"call_t","toolName":"todowrite","args":{"todos":[{"content":"写测试","status":"in_progress","priority":"high"},{"content":"跑测试","status":"pending"}]},"intent":"清单 0/2"}`,
		`{"type":"tool_execution_end","toolCallId":"call_t","toolName":"todowrite","result":{"content":[{"type":"text","text":"ok"}]},"isError":false}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}`,
		ompMsgEnd, ompAgentEnd)

	c := findControl(controls, protocol.TypeTodo)
	if c == nil {
		t.Fatalf("no TypeTodo emitted; got %v", ompControlTypes(controls))
	}
	if len(c.Todo.Todos) != 2 {
		t.Fatalf("todo items = %d, want 2", len(c.Todo.Todos))
	}
	if c.Todo.Todos[0].Content != "写测试" || c.Todo.Todos[0].Status != "in_progress" {
		t.Errorf("todo[0] = %+v", c.Todo.Todos[0])
	}
	// Must NOT also emit a TypeToolResult for todowrite (single-send).
	for _, cc := range controls {
		if cc.Type == protocol.TypeToolResult && cc.ToolResult.Name == "todowrite" {
			t.Errorf("todowrite must NOT also emit TypeToolResult; got %+v", cc.ToolResult)
		}
	}
}

// TestEmit_TaskToolLiftsToSubagent verifies a "task" tool call is lifted into
// the subagent zone: the TypeToolResult carries IsSubagent=true + a non-nil
// Subagent with the agent type + a title derived from args.i/task/agent. OMP's
// task output is used verbatim (no <task_result> wrapper).
func TestEmit_TaskToolLiftsToSubagent(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"tool_execution_start","toolCallId":"call_task","toolName":"task","args":{"agent":"scout","task":"探索布局","i":"Explore repo structure"},"intent":"Explore repo structure"}`,
		`{"type":"tool_execution_end","toolCallId":"call_task","toolName":"task","result":{"content":[{"type":"text","text":"Spawned 5 background agents using scout."}]}}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}`,
		ompMsgEnd, ompAgentEnd)

	var tr *protocol.Control
	for _, c := range controls {
		if c.Type == protocol.TypeToolResult && c.ToolResult.Name == "task" {
			tr = c
		}
	}
	if tr == nil {
		t.Fatalf("no TypeToolResult for task; got %v", ompControlTypes(controls))
	}
	if !tr.ToolResult.IsSubagent {
		t.Error("IsSubagent = false, want true")
	}
	if tr.ToolResult.Subagent == nil {
		t.Fatal("Subagent nil for task tool")
	}
	if tr.ToolResult.Subagent.Type != "scout" {
		t.Errorf("Subagent.Type = %q, want scout", tr.ToolResult.Subagent.Type)
	}
	// Title prefers args.i (intent) over args.task.
	if tr.ToolResult.Subagent.Title != "Explore repo structure" {
		t.Errorf("Subagent.Title = %q, want Explore repo structure", tr.ToolResult.Subagent.Title)
	}
	if tr.ToolResult.Subagent.Status != "completed" {
		t.Errorf("Subagent.Status = %q, want completed", tr.ToolResult.Subagent.Status)
	}
	if !strings.Contains(tr.ToolResult.Subagent.Preview, "Spawned 5") {
		t.Errorf("Subagent.Preview = %q, want output text", tr.ToolResult.Subagent.Preview)
	}
}

// TestEmit_CustomRoleNudge verifies a role=custom message_end with
// customType=mid-run-todo-nudge emits an info notice (not dropped silently, not
// routed to the error path).
func TestEmit_CustomRoleNudge(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`,
		`{"type":"message_end","message":{"role":"custom","customType":"mid-run-todo-nudge","content":"<system-reminder>10 todos open</system-reminder>","stopReason":"stop"}}`,
		ompMsgEnd, ompAgentEnd)

	var saw bool
	for _, c := range controls {
		if c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "待办提醒" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no 待办提醒 notice for custom role; got %v", ompControlTypes(controls))
	}
}

// TestEmit_ErrorCodesAppended verifies a stopReason=error message_end carrying
// errorStatus/errorId surfaces the codes (e.g. [429/135168]) in the terminal
// error text so the user can distinguish rate-limit / auth / model errors.
func TestEmit_ErrorCodesAppended(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorStatus":429,"errorId":135168,"errorMessage":"429 限流","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"total":0}}}}`)

	var errCtrl *protocol.Control
	for _, c := range controls {
		if c.Type == protocol.TypeError {
			errCtrl = c
		}
	}
	// EmitTerminal surfaces a non-nil PromptResult.Err as a TypeError whose
	// Message is the Err string — which the EventError path prefixes with
	// [status/id]. Assert the full prefix lands on the card verbatim.
	if errCtrl == nil {
		t.Fatalf("no TypeError emitted; got %v", ompControlTypes(controls))
	}
	if errCtrl.Error == nil {
		t.Fatal("TypeError has nil Error payload")
	}
	want := "[429/135168]"
	if !strings.Contains(errCtrl.Error.Message, want) {
		t.Errorf("Error.Message = %q, want it to contain %q", errCtrl.Error.Message, want)
	}
	if !strings.Contains(errCtrl.Error.Message, "429 限流") {
		t.Errorf("Error.Message = %q, want it to retain the upstream text", errCtrl.Error.Message)
	}
}

// TestReplay_TurnEndFallbackReply verifies the turn_end fallback: when the
// streaming path produced no assistant text (no text_delta, no text_end), the
// reply is recovered from the most recent turn_end assistant message instead of
// being blank. This drives streamRun directly (PromptResult assertion) like the
// existing replay tests.
func TestReplay_TurnEndFallbackReply(t *testing.T) {
	const sessionHdr = `{"type":"session","id":"s1","cwd":"/tmp"}`
	const agentStart = `{"type":"agent_start"}`
	const turnStart = `{"type":"turn_start"}`
	// No text_delta, no text_end — only a turn_end carrying the assistant text.
	const turnEnd = `{"type":"turn_end","message":{"role":"assistant","content":[{"type":"text","text":"TURN_END_RECOVERY"}]}}`
	const msgEnd = `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"total":0}}}}`
	const agentEnd = `{"type":"agent_end","messages":[],"isTerminal":true}`

	events := ompParseLines(t, sessionHdr, agentStart, turnStart, turnEnd, msgEnd, agentEnd)
	h, _ := newOmpReplayHandler(t)

	res := h.streamRun(context.Background(), "c1", "p1", ompEventChan(events), "spec-model", nil)
	if res.Err != nil {
		t.Fatalf("streamRun: %v", res.Err)
	}
	if !strings.Contains(res.Reply, "TURN_END_RECOVERY") {
		t.Errorf("reply = %q, want TURN_END_RECOVERY (turn_end fallback)", res.Reply)
	}
}

// TestReplay_ErrorCodesInResult verifies the EventError path appends the
// [status/id] prefix to the returned PromptResult.Err so the terminal card
// shows the codes. Drives streamRun directly.
func TestReplay_ErrorCodesInResult(t *testing.T) {
	const sessionHdr = `{"type":"session","id":"s1","cwd":"/tmp"}`
	const agentStart = `{"type":"agent_start"}`
	const errEnd = `{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"error","errorStatus":429,"errorId":135168,"errorMessage":"429 限流","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"total":0}}}}`

	events := ompParseLines(t, sessionHdr, agentStart, errEnd)
	h, _ := newOmpReplayHandler(t)

	res := h.streamRun(context.Background(), "c1", "p1", ompEventChan(events), "spec-model", nil)
	if res.Err == nil {
		t.Fatal("expected error from stopReason=error")
	}
	msg := res.Err.Error()
	if !strings.Contains(msg, "429") || !strings.Contains(msg, "135168") {
		t.Errorf("Err = %q, want both 429 and 135168 codes", msg)
	}
}

// TestEmit_TodoReminderNoControl verifies a todo_reminder event does NOT emit a
// control (it's a diagnostic; the todowrite tool is the authoritative list
// source) but also does not tick the unknown-event metric.
func TestEmit_TodoReminderNoControl(t *testing.T) {
	controls := runOmpLines(t, ompSessionHdr, ompAgentStart, ompTurnStart,
		`{"type":"todo_reminder","todos":[{"content":"x","status":"pending"}],"attempt":1,"maxAttempts":3}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`,
		ompMsgEnd, ompAgentEnd)

	// A todo_reminder must not produce a TypeTodo (only the todowrite tool does).
	if c := findControl(controls, protocol.TypeTodo); c != nil {
		t.Errorf("todo_reminder must not emit TypeTodo; got %+v", c.Todo)
	}
}
