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

// TestValidateTurnControls pins TypeTurnStarted / TypeTurnFinished requirements.
func TestValidateTurnControls(t *testing.T) {
	if err := (&Control{Type: TypeTurnStarted}).Validate(); err == nil {
		t.Fatal("TypeTurnStarted without payload should fail validation")
	}
	if err := (&Control{Type: TypeTurnStarted, TurnStarted: &TurnStartedPayload{}}).Validate(); err == nil {
		t.Fatal("TypeTurnStarted without promptID/chatID should fail validation")
	}
	okStart := &Control{Type: TypeTurnStarted, TurnStarted: &TurnStartedPayload{TurnInfo: TurnInfo{PromptID: "p1", ChatID: "c1"}}}
	if err := okStart.Validate(); err != nil {
		t.Fatalf("valid TypeTurnStarted should pass, got %v", err)
	}

	if err := (&Control{Type: TypeTurnFinished}).Validate(); err == nil {
		t.Fatal("TypeTurnFinished without payload should fail validation")
	}
	if err := (&Control{Type: TypeTurnFinished, TurnFinished: &TurnFinishedPayload{}}).Validate(); err == nil {
		t.Fatal("TypeTurnFinished without promptID should fail validation")
	}
	okFinish := &Control{Type: TypeTurnFinished, TurnFinished: &TurnFinishedPayload{PromptID: "p1"}}
	if err := okFinish.Validate(); err != nil {
		t.Fatalf("valid TypeTurnFinished should pass, got %v", err)
	}
}

// TestTurnControlRoundTrip pins the wire shape for turn lifecycle controls.
func TestTurnControlRoundTrip(t *testing.T) {
	start := &Control{
		Type:     TypeTurnStarted,
		PromptID: "p1",
		ChatID:   "c1",
		TurnStarted: &TurnStartedPayload{TurnInfo: TurnInfo{
			PromptID:  "p1",
			ChatID:    "c1",
			BackendID: "b1",
			ElapsedS:  0,
		}},
	}
	gotStart := roundTrip(t, start)
	if gotStart.Type != TypeTurnStarted || gotStart.TurnStarted == nil || gotStart.TurnStarted.PromptID != "p1" {
		t.Fatalf("turn_started round trip: %+v", gotStart)
	}

	finish := &Control{
		Type:         TypeTurnFinished,
		PromptID:     "p1",
		TurnFinished: &TurnFinishedPayload{PromptID: "p1"},
	}
	gotFinish := roundTrip(t, finish)
	if gotFinish.Type != TypeTurnFinished || gotFinish.TurnFinished == nil || gotFinish.TurnFinished.PromptID != "p1" {
		t.Fatalf("turn_finished round trip: %+v", gotFinish)
	}
}
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

	if err := (&Control{Type: TypePong}).Validate(); err == nil {
		t.Error("TypePong without payload should fail")
	}
	if err := (&Control{Type: TypePong, Pong: &PongPayload{}}).Validate(); err != nil {
		t.Errorf("TypePong with payload should pass (no chatID needed), got %v", err)
	}
}
