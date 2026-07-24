package opencodeservebridge

import (
	"context"
	"testing"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
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

// eventChan buffers events into a closed channel the way a real SDK Run would,
// so streamRun can be driven directly without a live serve connection.
func eventChan(events []oc.HighEvent) <-chan oc.HighEvent {
	ch := make(chan oc.HighEvent, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

// TestStreamRun_AccumulatesCostAndTokensAcrossSteps verifies that a multi-step
// turn (N tool-calls steps + a terminal stop step) sums every step's tokens
// AND cost into the result. Previously only the terminal step's cost was kept,
// undercounting cost on tool-heavy turns while tokens were already summed.
func TestStreamRun_AccumulatesCostAndTokensAcrossSteps(t *testing.T) {
	// 两个非终止 step_finish（finish=tool-calls）+ 一个终止 result（finish=stop）。
	toolStep := oc.NewHighEvent(oc.HighEventStepFinish, "s1", "m1",
		oc.WithTokens(200, 80, 400, 0), oc.WithCost(0.01))
	stopStep := oc.NewHighEvent(oc.HighEventResult, "s1", "m1",
		oc.WithTokens(1000, 500, 300, 50), oc.WithCost(0.02), oc.WithResult("stop"))

	events := []oc.HighEvent{toolStep, toolStep, stopStep}
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "")

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
	if res.reply == "" {
		t.Error("multi-step turn should yield non-empty reply")
	}
}

// TestStreamRun_SingleStepCostIsTerminal guards the single-step turn: no
// accumulation, the result cost equals the sole stop step's cost.
func TestStreamRun_SingleStepCostIsTerminal(t *testing.T) {
	stopStep := oc.NewHighEvent(oc.HighEventResult, "s1", "m1",
		oc.WithTokens(1000, 500, 300, 50), oc.WithCost(0.02), oc.WithResult("stop"))

	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan([]oc.HighEvent{stopStep}), "")
	if res.costUSD != 0.02 {
		t.Errorf("costUSD = %v, want 0.02", res.costUSD)
	}
}

// scriptStreamOpencode yields a fixed slice of events then closes, so a test
// can drive streamRun through a specific event sequence (closedStreamOpencode
// embeds the no-op method set; only Run is overridden).
type scriptStreamOpencode struct {
	closedStreamOpencode
	events []oc.HighEvent
}

func (s scriptStreamOpencode) Run(_ context.Context, _ oc.RunOptions) (<-chan oc.HighEvent, error) {
	ch := make(chan oc.HighEvent, len(s.events))
	for _, e := range s.events {
		ch <- e
	}
	close(ch)
	return ch, nil
}

// TestStreamRun_TodoUpdatedEmitsTypeTodoControl verifies the SDK todo_updated
// event is field-copied into a TypeTodo Control: the protocol package never
// imports the SDK (the copy happens here), and content/status/priority survive
// the translation intact.
func TestStreamRun_TodoUpdatedEmitsTypeTodoControl(t *testing.T) {
	todoEv := oc.NewHighEvent(oc.HighEventTodoUpdated, "s1", "m1",
		oc.WithTodoUpdated(&oc.TodoUpdatedData{Todos: []oc.Todo{
			{Content: "任务一", Status: "completed", Priority: "high"},
			{Content: "任务二", Status: "in_progress"},
		}}))
	termEv := oc.NewHighEvent(oc.HighEventResult, "s1", "m1", oc.WithResult("done"))

	client, reg, cleanup := connectTestRPC(t)
	defer cleanup()
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, scriptStreamOpencode{events: []oc.HighEvent{todoEv, termEv}}, client, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c-todo", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-todo",
		Prompt:   &protocol.PromptPayload{ChatID: "c-todo", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	controls := drainUntilTerminal(t, reg)
	var todo *protocol.Control
	for _, c := range controls {
		if c.Type == protocol.TypeTodo {
			todo = c
			break
		}
	}
	if todo == nil {
		t.Fatalf("no TypeTodo control emitted; got %v", controlTypes(controls))
	}
	if todo.Todo == nil || len(todo.Todo.Todos) != 2 {
		t.Fatalf("todo payload = %+v, want 2 items", todo.Todo)
	}
	if todo.Todo.Todos[0] != (protocol.TodoItem{Content: "任务一", Status: "completed", Priority: "high"}) {
		t.Errorf("todos[0] = %+v", todo.Todo.Todos[0])
	}
	if todo.Todo.Todos[1].Content != "任务二" || todo.Todo.Todos[1].Status != "in_progress" {
		t.Errorf("todos[1] = %+v", todo.Todo.Todos[1])
	}
}
