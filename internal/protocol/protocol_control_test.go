package protocol

import "testing"

// TestTodoControlRoundTrip pins the TypeTodo wire shape: a Control carrying a
// Todo payload survives marshal/unmarshal with its todos intact (content /
// status / priority), so the renderer sees the same list the backend sent.
func TestTodoControlRoundTrip(t *testing.T) {
	in := &Control{
		Type: TypeTodo,
		Todo: &TodoPayload{Todos: []TodoItem{
			{Content: "写测试", Status: "in_progress", Priority: "high"},
			{Content: "提交", Status: "pending"},
		}},
	}
	got := roundTrip(t, in)
	if got.Type != TypeTodo || got.Todo == nil {
		t.Fatalf("todo round trip: %+v", got)
	}
	if len(got.Todo.Todos) != 2 {
		t.Fatalf("todos len = %d, want 2", len(got.Todo.Todos))
	}
	if got.Todo.Todos[0].Content != "写测试" || got.Todo.Todos[0].Status != "in_progress" || got.Todo.Todos[0].Priority != "high" {
		t.Errorf("todos[0] = %+v", got.Todo.Todos[0])
	}
}

// TestValidateTodoControl covers the two Validate branches for TypeTodo: a
// missing payload is rejected, a present one passes (todo rides the progress
// card, so it does NOT require a chatID unlike Question/Notice).
func TestValidateTodoControl(t *testing.T) {
	if err := (&Control{Type: TypeTodo}).Validate(); err == nil {
		t.Fatal("TypeTodo without payload should fail validation")
	}
	if err := (&Control{Type: TypeTodo, Todo: &TodoPayload{}}).Validate(); err != nil {
		t.Fatalf("TypeTodo with payload should pass, got %v", err)
	}
}

// TestValidateEnums (C11): gate kind, task type, and the C2 pong control
// are pinned to their known enum sets; unknown values fail loudly instead
// of being silently mis-rendered.
func TestValidateEnums(t *testing.T) {
	okGate := &Control{Type: TypeProgress, Progress: &ProgressPayload{
		Gate: &GateInfo{State: "waiting", Kind: "permission"},
	}}
	if err := okGate.Validate(); err != nil {
		t.Errorf("valid gate: %v", err)
	}
	badGate := &Control{Type: TypeProgress, Progress: &ProgressPayload{
		Gate: &GateInfo{State: "waiting", Kind: "banana"},
	}}
	if err := badGate.Validate(); err == nil {
		t.Error("gate.kind banana should fail")
	}

	okSub := &Control{Type: TypeToolUse, ToolUse: &ToolUsePayload{
		Name: "Task", Subagent: &SubagentSummary{Status: "running", TaskType: "local_agent"},
	}}
	if err := okSub.Validate(); err != nil {
		t.Errorf("valid subagent: %v", err)
	}
	badSub := &Control{Type: TypeToolUse, ToolUse: &ToolUsePayload{
		Name: "Task", Subagent: &SubagentSummary{Status: "running", TaskType: "warp_drive"},
	}}
	if err := badSub.Validate(); err == nil {
		t.Error("taskType warp_drive should fail")
	}

	if err := (&Control{Type: TypePong}).Validate(); err == nil {
		t.Error("TypePong without payload should fail")
	}
	if err := (&Control{Type: TypePong, Pong: &PongPayload{}}).Validate(); err != nil {
		t.Errorf("TypePong with payload should pass (no chatID needed), got %v", err)
	}
}
