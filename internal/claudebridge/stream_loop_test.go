package claudebridge

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/claude"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

func TestSummarizeToolInput_Subject(t *testing.T) {
	// TaskCreate carries subject (short title) and description (long paragraph);
	// subject must win so the card shows the title, not the paragraph.
	input := `{"subject":"梳理架构","description":"通过阅读源码全面理解...","activeForm":"正在梳理"}`
	if got := bridgebase.SummarizeToolInput("", input); got != "梳理架构" {
		t.Errorf("summarizeToolInput subject = %q, want 梳理架构", got)
	}
}

func TestSummarizeToolInput_FilePath(t *testing.T) {
	input := `{"file_path":"/opt/codes/README.md"}`
	if got := bridgebase.SummarizeToolInput("", input); got != "/opt/codes/README.md" {
		t.Errorf("summarizeToolInput = %q", got)
	}
}

func TestSummarizeToolInput_MCPFields(t *testing.T) {
	// MCP tools pass server-defined params the common keys don't cover;
	// repo_path / project must be picked up so the row isn't bare.
	tests := []struct {
		name, input, want string
	}{
		{"repo_path", `{"repo_path":"/opt/codes/lark-bridge","mode":"full"}`, "/opt/codes/lark-bridge"},
		{"project", `{"project":"lark-bridge"}`, "lark-bridge"},
		{"url", `{"url":"https://example.com/x"}`, "https://example.com/x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgebase.SummarizeToolInput("", tc.input); got != tc.want {
				t.Errorf("bridgebase.SummarizeToolInput(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestSummarizeToolInput_ToolIdentifiers(t *testing.T) {
	// TaskUpdate/Skill carry several string fields none of the generic keys
	// cover; without taskId/skill in the priority table the summary would
	// non-deterministically pick status/args (the map-iteration fallback),
	// showing the user the wrong value.
	tests := []struct {
		name, input, want string
	}{
		{"task taskId", `{"status":"in_progress","taskId":"1"}`, "1"},
		{"skill name", `{"skill":"codebase-memory","args":"explore the repo"}`, "codebase-memory"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bridgebase.SummarizeToolInput("", tc.input); got != tc.want {
				t.Errorf("bridgebase.SummarizeToolInput(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

func TestSummarizeToolInput_FirstStringValueFallback(t *testing.T) {
	// Unrecognised tool with no common key: the first string value beats
	// returning the raw JSON.
	input := `{"foo":"bar","count":3}`
	if got := bridgebase.SummarizeToolInput("", input); got != "bar" {
		t.Errorf("summarizeToolInput = %q, want bar (first string value)", got)
	}
}

// TestStreamRun_ToolResultNameCorrelatedByFeed locks in the id→name lookup:
// claude emits tool_use (with name) then a separate tool_result (carrying only
// the id). The ToolResult Control must carry the name so the progress card can
// match the row — without it a failed tool shows no command.
func TestStreamRun_ToolResultNameCorrelatedByFeed(t *testing.T) {
	// Real stream-json shapes: tool_use carries name+id, tool_result carries
	// only the id. Built via ParseEvent so the test exercises the real parse
	// path rather than hand-building Event structs.
	useEvents, err := claude.ParseEvent(`{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"git push"}}]}}`)
	if err != nil {
		t.Fatalf("parse tool_use: %v", err)
	}
	resultEvents, err := claude.ParseEvent(`{"type":"user","session_id":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"This command requires approval","is_error":true}]}}`)
	if err != nil {
		t.Fatalf("parse tool_result: %v", err)
	}
	termEvents, err := claude.ParseEvent(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	events := append(append(append([]claude.Event{}, useEvents...), resultEvents...), termEvents...)

	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptClaude{events: events}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c-tool", "", t.TempDir(), "", "", "")

	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-tool",
		Prompt:   &protocol.PromptPayload{ChatID: "c-tool", Text: "hi"},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// With fire-and-forget emit the terminal Result (sent synchronously by
	// emitTerminal) can arrive before the async ToolResult goroutine
	// completes, so drain all controls until the terminal and then assert.
	controls := drainUntilTerminal(t, reg)
	var toolResult *protocol.Control
	for _, c := range controls {
		if c.Type == protocol.TypeToolResult {
			toolResult = c
			break
		}
	}
	if toolResult == nil {
		t.Fatalf("no ToolResult control received; got %d controls: %v", len(controls), controlTypes(controls))
	}
	if toolResult.ToolResult.Name != "Bash" {
		t.Fatalf("ToolResult Name = %q, want Bash (correlated from tool_use id)", toolResult.ToolResult.Name)
	}
	if !toolResult.ToolResult.IsError {
		t.Errorf("IsError = false, want true")
	}
}

// TestStreamRun_TaskProgressFoldedIntoToolRow locks in subagent progress
// surfacing: task_started opens a running "Explore Agent" row, task_progress
// updates its description (via a re-emitted ToolUse), and task_notification
// closes the row with a cumulative-usage line. Without this, all 66 progress
// ticks in a real Explore turn are dropped silently.
func TestStreamRun_TaskProgressFoldedIntoToolRow(t *testing.T) {
	started, _ := claude.ParseEvent(`{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu_1","description":"Explore codebase architecture","subagent_type":"Explore","task_type":"local_agent","prompt":"x","session_id":"s1"}`)
	progress, _ := claude.ParseEvent(`{"type":"system","subtype":"task_progress","task_id":"t1","tool_use_id":"tu_1","description":"Reading internal/opencode/model.go","subagent_type":"Explore","usage":{"total_tokens":104609,"tool_uses":65,"duration_ms":59675},"last_tool_name":"Read","session_id":"s1"}`)
	notify, _ := claude.ParseEvent(`{"type":"system","subtype":"task_notification","task_id":"t1","tool_use_id":"tu_1","status":"completed","output_file":"","summary":"Explore codebase architecture","usage":{"total_tokens":107296,"tool_uses":66,"duration_ms":98342},"session_id":"s1"}`)
	term, _ := claude.ParseEvent(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`)
	events := append(append(append(append([]claude.Event{}, started...), progress...), notify...), term...)

	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptClaude{events: events}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c-task", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-task",
		Prompt:   &protocol.PromptPayload{ChatID: "c-task", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// With fire-and-forget emit, drain all controls until the terminal
	// Result, then assert the subagent controls were all delivered.
	controls := drainUntilTerminal(t, reg)
	var sawUseStart, sawUseProgress, sawResult bool
	for _, c := range controls {
		switch c.Type {
		case protocol.TypeToolUse:
			if c.ToolUse.Name != "Explore Agent" {
				t.Errorf("ToolUse Name = %q, want Explore Agent", c.ToolUse.Name)
			}
			if c.ToolUse.TaskID != "t1" {
				t.Errorf("ToolUse TaskID = %q, want t1 (correlates lifecycle)", c.ToolUse.TaskID)
			}
			if strings.Contains(c.ToolUse.Input, "Reading internal/opencode/model.go") {
				sawUseStart = true // first use carries the task title
			} else {
				sawUseProgress = true
			}
		case protocol.TypeToolResult:
			// Notification closes the row; cumulative usage rides on Input
			// (the row description), Output is empty — the progress card shows
			// actions, not tool output. TaskID lets the frontend close the
			// exact row opened by task_started regardless of name/desc drift.
			if c.ToolResult.Output != "" {
				t.Errorf("ToolResult Output = %q, want empty", c.ToolResult.Output)
			}
			if c.ToolResult.TaskID != "t1" {
				t.Errorf("ToolResult TaskID = %q, want t1", c.ToolResult.TaskID)
			}
			if !strings.Contains(c.ToolResult.Input, "66步") || !strings.Contains(c.ToolResult.Input, "107k tokens") {
				t.Errorf("ToolResult Input = %q, want cumulative usage (66步, 107k tokens)", c.ToolResult.Input)
			}
			sawResult = true
		}
	}
	if !sawUseStart || !sawUseProgress || !sawResult {
		t.Fatalf("missed subagent controls: start=%v progress=%v result=%v (got %d controls: %v)",
			sawUseStart, sawUseProgress, sawResult, len(controls), controlTypes(controls))
	}
}

// TestStreamRun_UnknownEventNotFatal ensures an unrecognised stream-json type
// is logged, not fatal: the parser forwards it verbatim, the bridge default
// branch emits a debug log, and the turn still completes normally.
func TestStreamRun_UnknownEventNotFatal(t *testing.T) {
	unknown, _ := claude.ParseEvent(`{"type":"future_event","subtype":"x","session_id":"s1"}`)
	term, _ := claude.ParseEvent(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s1"}`)
	events := append([]claude.Event{}, unknown...)
	events = append(events, term...)

	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptClaude{events: events}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())
	r.Bind("c-unk", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-unk",
		Prompt:   &protocol.PromptPayload{ChatID: "c-unk", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Drain to the terminal result; the unknown event must not produce an
	// error control or abort the turn.
	for {
		ctrl := drainControl(t, reg)
		if ctrl.Type == protocol.TypeResult {
			if ctrl.Result.Text != "ok" {
				t.Errorf("result = %q, want ok", ctrl.Result.Text)
			}
			return
		}
		if ctrl.Type == protocol.TypeError {
			t.Fatalf("unknown event surfaced as error: %+v", ctrl)
		}
	}
}

// TestStreamRun_TodoWriteEmitsTypeTodoNotToolUse pins the single-send
// behaviour for claude's TodoWrite tool: tool_use is buffered (no TypeToolUse
// row opened), the matching tool_result is rewritten to a TypeTodo control
// (so the progress card's todo zone renders ✅/⏳/⬜/✘ rows), and NO
// TypeToolUse / TypeToolResult is emitted for it. The tool_use input shape
// `{"todos":[...]}` matches protocol.TodoItem 1:1.
func TestStreamRun_TodoWriteEmitsTypeTodoNotToolUse(t *testing.T) {
	// Real claude stream-json shape: tool_use carries name+id+input, the
	// later tool_result carries only tool_use_id (no name).
	useEvents, err := claude.ParseEvent(`{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_todo","name":"TodoWrite","input":{"todos":[{"content":"写测试","status":"in_progress","priority":"high"},{"content":"跑测试","status":"pending"}]}}]}}`)
	if err != nil {
		t.Fatalf("parse tool_use: %v", err)
	}
	resultEvents, err := claude.ParseEvent(`{"type":"user","session_id":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_todo","content":"ok","is_error":false}]}}`)
	if err != nil {
		t.Fatalf("parse tool_result: %v", err)
	}
	termEvents, err := claude.ParseEvent(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	events := append(append(append([]claude.Event{}, useEvents...), resultEvents...), termEvents...)

	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptClaude{events: events}, client, HandlerConfig{
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
		if c.Type == protocol.TypeToolUse && c.ToolUse.Name == "TodoWrite" {
			t.Errorf("TodoWrite must NOT emit TypeToolUse (row suppressed): %+v", c.ToolUse)
		}
		if c.Type == protocol.TypeToolResult && c.ToolResult.Name == "TodoWrite" {
			t.Errorf("TodoWrite success must NOT emit TypeToolResult (single-send to TypeTodo): %+v", c.ToolResult)
		}
	}
	if !sawTodo {
		t.Fatalf("no TypeTodo control received; got %d controls: %v", len(controls), controlTypes(controls))
	}
}

// TestStreamRun_TodoWriteFailureFallsBackToToolResult locks the fallback: a
// failed TodoWrite (is_error=true) is NOT rewritten to TypeTodo (the list did
// not update); it ships as a TypeToolResult so the user sees the failure on
// the card. The TypeToolUse was suppressed at tool_use time, so AddToolResult
// renders a no-prior-row result as a standalone row.
func TestStreamRun_TodoWriteFailureFallsBackToToolResult(t *testing.T) {
	useEvents, err := claude.ParseEvent(`{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_fail","name":"TodoWrite","input":{"todos":[{"content":"x","status":"pending"}]}}]}}`)
	if err != nil {
		t.Fatalf("parse tool_use: %v", err)
	}
	resultEvents, err := claude.ParseEvent(`{"type":"user","session_id":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_fail","content":"db locked","is_error":true}]}}`)
	if err != nil {
		t.Fatalf("parse tool_result: %v", err)
	}
	termEvents, err := claude.ParseEvent(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	events := append(append(append([]claude.Event{}, useEvents...), resultEvents...), termEvents...)

	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptClaude{events: events}, client, HandlerConfig{
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
			t.Errorf("failed TodoWrite must NOT emit TypeTodo; got %+v", c.Todo)
		}
		if c.Type == protocol.TypeToolResult && c.ToolResult.Name == "TodoWrite" {
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
		t.Fatalf("no TypeToolResult for failed TodoWrite; got %d controls: %v", len(controls), controlTypes(controls))
	}
}

// TestStreamRun_OtherToolsUnaffectedByTodoRouting guards the side-effect
// boundary: a non-TodoWrite tool (Bash) still emits the TypeToolUse +
// TypeToolResult pair exactly as before — the TodoWrite→TypeTodo rewrite is
// gated on the tool name.
func TestStreamRun_OtherToolsUnaffectedByTodoRouting(t *testing.T) {
	useEvents, err := claude.ParseEvent(`{"type":"assistant","session_id":"s1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"ls"}}]}}`)
	if err != nil {
		t.Fatalf("parse tool_use: %v", err)
	}
	resultEvents, err := claude.ParseEvent(`{"type":"user","session_id":"s1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_bash","content":"file.txt","is_error":false}]}}`)
	if err != nil {
		t.Fatalf("parse tool_result: %v", err)
	}
	termEvents, err := claude.ParseEvent(`{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"s1"}`)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	events := append(append(append([]claude.Event{}, useEvents...), resultEvents...), termEvents...)

	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptClaude{events: events}, client, HandlerConfig{
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
	var sawBashUse, sawBashResult bool
	for _, c := range controls {
		if c.Type == protocol.TypeTodo {
			t.Errorf("Bash tool must NOT emit TypeTodo; got %+v", c.Todo)
		}
		if c.Type == protocol.TypeToolUse && c.ToolUse.Name == "Bash" {
			sawBashUse = true
		}
		if c.Type == protocol.TypeToolResult && c.ToolResult.Name == "Bash" {
			sawBashResult = true
		}
	}
	if !sawBashUse || !sawBashResult {
		t.Fatalf("missed Bash controls: use=%v result=%v (got %d controls: %v)",
			sawBashUse, sawBashResult, len(controls), controlTypes(controls))
	}
}
