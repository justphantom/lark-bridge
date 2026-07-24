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
