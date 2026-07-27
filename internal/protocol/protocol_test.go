package protocol

import (
	"encoding/json"
	"testing"
)

// roundTrip marshals v and unmarshals into a fresh zero value, asserting no
// error. Returns the unmarshaled copy.
func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got T
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

// TestEventRoundTrip covers a positive marshal/unmarshal for each event type.
func TestEventRoundTrip(t *testing.T) {
	ping := &PingPayload{}
	got := roundTrip(t, &Event{Type: TypePing, PromptID: "p", Ping: ping})
	if got.Type != TypePing || got.Ping == nil {
		t.Fatalf("ping round trip: %+v", got)
	}
}

func TestPromptEventRoundTrip(t *testing.T) {
	got := roundTrip(t, &Event{
		Type:     TypePrompt,
		PromptID: "msg-1",
		Prompt:   &PromptPayload{ChatID: "c1", Text: "hello", Agent: "build"},
	})
	if got.Type != TypePrompt || got.Prompt.Text != "hello" || got.Prompt.Agent != "build" {
		t.Fatalf("prompt round trip: %+v", got)
	}
}

func TestAnswerEventRoundTrip(t *testing.T) {
	got := roundTrip(t, &Event{
		Type:     TypeAnswer,
		PromptID: "msg-1",
		Answer:   &AnswerPayload{ChatID: "c1", RequestID: "r1", Choices: []string{"a", "b"}, Custom: "x"},
	})
	if got.Type != TypeAnswer || got.Answer.RequestID != "r1" || len(got.Answer.Choices) != 2 || got.Answer.Custom != "x" {
		t.Fatalf("answer round trip: %+v", got)
	}
}

func TestAbortEventRoundTrip(t *testing.T) {
	got := roundTrip(t, &Event{
		Type:     TypeAbort,
		PromptID: "msg-1",
		Abort:    &AbortPayload{ChatID: "c1", SessionID: "s1"},
	})
	if got.Type != TypeAbort || got.Abort.ChatID != "c1" {
		t.Fatalf("abort round trip: %+v", got)
	}
}

// TestControlRoundTrip covers a positive marshal/unmarshal for each control
// type.
func TestControlRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		ctrl *Control
	}{
		{"session_init", &Control{Type: TypeSessionInit, SessionInit: &SessionInitPayload{SessionID: "s1", Model: "m"}}},
		{"text", &Control{Type: TypeText, Text: &TextPayload{Delta: "hi"}}},
		{"thinking", &Control{Type: TypeThinking, Thinking: &ThinkingPayload{Delta: "hmm"}}},
		{"tool_use", &Control{Type: TypeToolUse, ToolUse: &ToolUsePayload{Name: "bash", Input: "ls"}}},
		{"tool_result", &Control{Type: TypeToolResult, ToolResult: &ToolResultPayload{Name: "bash", Output: "ok", IsError: true}}},
		{"result", &Control{Type: TypeResult, Result: &ResultPayload{Text: "done", Model: "m", Tokens: 10}}},
		{"error", &Control{Type: TypeError, Error: &ErrorPayload{Message: "boom", Recoverable: true}}},
		{"progress", &Control{Type: TypeProgress, Progress: &ProgressPayload{Description: "working"}}},
		{"question", &Control{Type: TypeQuestion, ChatID: "c1", Question: &QuestionPayload{RequestID: "r1", PromptID: "p", Questions: []QuestionItem{{Label: "q", Options: []string{"a"}}}}}},
		{"permission", &Control{Type: TypePermission, ChatID: "c1", Permission: &PermissionPayload{RequestID: "r1", PromptID: "p", Options: []PermissionOption{{Label: "允许", Value: "allow"}}}}},
		{"notice", &Control{Type: TypeNotice, ChatID: "c1", Notice: &NoticePayload{Level: "info", Title: "t", Message: "m"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := roundTrip(t, tc.ctrl)
			if got.Type != tc.ctrl.Type {
				t.Fatalf("type lost: got %q want %q", got.Type, tc.ctrl.Type)
			}
		})
	}
}

// TestEventValidate covers the Event validation rules from task 2.4/2.5.
func TestEventValidate(t *testing.T) {
	cases := []struct {
		name    string
		ev      *Event
		wantErr bool
	}{
		{"prompt missing payload", &Event{Type: TypePrompt, PromptID: "p"}, true},
		{"prompt missing chatID", &Event{Type: TypePrompt, PromptID: "p", Prompt: &PromptPayload{Text: "x"}}, true},
		{"prompt ok", &Event{Type: TypePrompt, PromptID: "p", Prompt: &PromptPayload{ChatID: "c1", Text: "x"}}, false},
		{"ping with payload ok", &Event{Type: TypePing, PromptID: "p", Ping: &PingPayload{}}, false},
		{"ping without promptID ok", &Event{Type: TypePing}, false},
		{"answer without promptID ok", &Event{Type: TypeAnswer, Answer: &AnswerPayload{ChatID: "c1", RequestID: "r1"}}, false},
		{"answer missing requestID", &Event{Type: TypeAnswer, Answer: &AnswerPayload{ChatID: "c1"}}, true},
		{"abort missing chatID", &Event{Type: TypeAbort, PromptID: "p", Abort: &AbortPayload{SessionID: "s1"}}, true},
		{"abort ok", &Event{Type: TypeAbort, PromptID: "p", Abort: &AbortPayload{ChatID: "c1"}}, false},
		{"unknown type", &Event{Type: "unknown"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ev.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

// TestControlValidate covers the Control validation rules from task 2.4/2.5.
func TestControlValidate(t *testing.T) {
	cases := []struct {
		name    string
		ctrl    *Control
		wantErr bool
	}{
		{"text missing payload", &Control{Type: TypeText}, true},
		{"text ok", &Control{Type: TypeText, Text: &TextPayload{Delta: "x"}}, false},
		{"thinking delta ok", &Control{Type: TypeThinking, Thinking: &ThinkingPayload{Delta: "hmm"}}, false},
		{"thinking replace ok", &Control{Type: TypeThinking, Thinking: &ThinkingPayload{Delta: "full", Replace: true}}, false},
		{"question missing chatID", &Control{Type: TypeQuestion, Question: &QuestionPayload{RequestID: "r", PromptID: "p", Questions: []QuestionItem{{Label: "q", Options: []string{"a"}}}}}, true},
		{"permission missing payload", &Control{Type: TypePermission, ChatID: "c1"}, true},
		{"permission missing chatID", &Control{Type: TypePermission, Permission: &PermissionPayload{RequestID: "r", Options: []PermissionOption{{Label: "a", Value: "a"}}}}, true},
		{"permission ok", &Control{Type: TypePermission, ChatID: "c1", Permission: &PermissionPayload{RequestID: "r", Options: []PermissionOption{{Label: "a", Value: "a"}}}}, false},
		{"notice missing chatID", &Control{Type: TypeNotice, Notice: &NoticePayload{Level: "info", Title: "t"}}, true},
		{"notice ok", &Control{Type: TypeNotice, ChatID: "c1", Notice: &NoticePayload{Level: "info", Title: "t"}}, false},
		{"notice bad level", &Control{Type: TypeNotice, ChatID: "c1", Notice: &NoticePayload{Level: "trace", Title: "t"}}, true},
		{"todo ok", &Control{Type: TypeTodo, Todo: &TodoPayload{Todos: []TodoItem{{Content: "x", Status: "pending", Priority: "high"}}}}, false},
		{"todo bad status", &Control{Type: TypeTodo, Todo: &TodoPayload{Todos: []TodoItem{{Content: "x", Status: "done"}}}}, true},
		{"todo bad priority", &Control{Type: TypeTodo, Todo: &TodoPayload{Todos: []TodoItem{{Content: "x", Status: "pending", Priority: "urgent"}}}}, true},
		{"todo empty priority ok", &Control{Type: TypeTodo, Todo: &TodoPayload{Todos: []TodoItem{{Content: "x", Status: "pending"}}}}, false},
		{"progress ok", &Control{Type: TypeProgress, Progress: &ProgressPayload{Description: "loading"}}, false},
		{"progress gate ok", &Control{Type: TypeProgress, Progress: &ProgressPayload{Gate: &GateInfo{State: "waiting", Kind: "permission", Summary: "Bash"}}}, false},
		{"progress gate bad state", &Control{Type: TypeProgress, Progress: &ProgressPayload{Gate: &GateInfo{State: "paused"}}}, true},
		{"tool_use nil subagent ok", &Control{Type: TypeToolUse, ToolUse: &ToolUsePayload{Name: "task"}}, false},
		{"tool_use subagent running ok", &Control{Type: TypeToolUse, ToolUse: &ToolUsePayload{Name: "task", IsSubagent: true, TaskID: "t1", Subagent: &SubagentSummary{Status: "running", TaskType: "local_agent", Type: "general-purpose", Title: "explore"}}}, false},
		{"tool_use subagent bad status", &Control{Type: TypeToolUse, ToolUse: &ToolUsePayload{Name: "task", Subagent: &SubagentSummary{Status: "paused"}}}, true},
		{"tool_result nil subagent ok", &Control{Type: TypeToolResult, ToolResult: &ToolResultPayload{Name: "task", Output: "ok"}}, false},
		{"tool_result subagent completed ok", &Control{Type: TypeToolResult, ToolResult: &ToolResultPayload{Name: "task", IsSubagent: true, TaskID: "t1", Subagent: &SubagentSummary{Status: "completed", Preview: "done", OutputBytes: 1024}}}, false},
		{"tool_result subagent failed ok", &Control{Type: TypeToolResult, ToolResult: &ToolResultPayload{Name: "task", IsError: true, IsSubagent: true, Subagent: &SubagentSummary{Status: "failed"}}}, false},
		{"tool_result subagent bad status", &Control{Type: TypeToolResult, ToolResult: &ToolResultPayload{Name: "task", Subagent: &SubagentSummary{Status: "ok"}}}, true},
		{"unknown type", &Control{Type: "unknown"}, true},
		// BackendID present but irrelevant to validation.
		{"text with backendID ok", &Control{Type: TypeText, BackendID: "b", Text: &TextPayload{Delta: "x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.ctrl.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected nil, got %v", err)
			}
		})
	}
}

// TestOmitemptyNoEmptyPayloadFields asserts serialised Event/Control omit nil
// payload fields (acceptance: no empty payload fields appear in the JSON).
func TestOmitemptyNoEmptyPayloadFields(t *testing.T) {
	data, err := json.Marshal(&Event{Type: TypePrompt, PromptID: "p"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	// A nil Answer/Abort/Ping must not appear at all.
	for _, key := range []string{`"answer"`, `"abort"`, `"ping"`} {
		if contains(s, key) {
			t.Errorf("Event JSON contains nil payload field %q: %s", key, s)
		}
	}

	cdata, err := json.Marshal(&Control{Type: TypeText, Text: &TextPayload{Delta: "x"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	cs := string(cdata)
	for _, key := range []string{`"sessionInit"`, `"result"`, `"error"`, `"notice"`} {
		if contains(cs, key) {
			t.Errorf("Control JSON contains nil payload field %q: %s", key, cs)
		}
	}
}

// TestSubagentRoundTrip locks in SubagentSummary's JSON shape: every backend
// field round-trips, omitempty drops zero values (claude omits Model/Truncated,
// opencode omits ToolUses/LastToolName), and the Subagent pointer threads
// through both ToolUsePayload and ToolResultPayload.
func TestSubagentRoundTrip(t *testing.T) {
	full := &SubagentSummary{
		Status:       "running",
		TaskType:     "local_agent",
		Type:         "general-purpose",
		Title:        "调查飞书接口调用",
		Description:  "Reading internal/lark/ws/frame.go",
		ChildSession: "a31153be0cabb040a",
		Model:        "glm-5.2",
		DurationMs:   33203,
		ToolUses:     14,
		LastToolName: "Read",
		TotalTokens:  0, // omitempty: dropped
		Preview:      "",
		OutputBytes:  0,     // omitempty: dropped
		Truncated:    false, // omitempty: dropped
	}
	got := roundTrip(t, full)
	if got.Status != "running" || got.TaskType != "local_agent" || got.Type != "general-purpose" {
		t.Fatalf("core fields lost: %+v", got)
	}
	if got.Title != "调查飞书接口调用" || got.Description != "Reading internal/lark/ws/frame.go" {
		t.Fatalf("text fields lost: %+v", got)
	}
	if got.DurationMs != 33203 || got.ToolUses != 14 || got.LastToolName != "Read" {
		t.Fatalf("numeric/usage fields lost: %+v", got)
	}
	// omitempty zero values must round-trip as zero (absent in JSON, default on unmarshal).
	if got.TotalTokens != 0 || got.OutputBytes != 0 || got.Truncated != false {
		t.Fatalf("zero fields should stay zero: %+v", got)
	}

	// Threading through ToolUsePayload (running/progress phase).
	tu := roundTrip(t, &Control{Type: TypeToolUse, ToolUse: &ToolUsePayload{
		Name: "Agent", IsSubagent: true, TaskID: "t1", Subagent: full,
	}})
	if tu.ToolUse.Subagent == nil || tu.ToolUse.Subagent.Status != "running" {
		t.Fatalf("ToolUse.Subagent lost: %+v", tu.ToolUse)
	}

	// Threading through ToolResultPayload (terminal phase).
	tr := roundTrip(t, &Control{Type: TypeToolResult, ToolResult: &ToolResultPayload{
		Name: "Agent", IsSubagent: true, TaskID: "t1",
		Subagent: &SubagentSummary{Status: "completed", Preview: "done", OutputBytes: 2067},
	}})
	if tr.ToolResult.Subagent == nil || tr.ToolResult.Subagent.Status != "completed" ||
		tr.ToolResult.Subagent.Preview != "done" || tr.ToolResult.Subagent.OutputBytes != 2067 {
		t.Fatalf("ToolResult.Subagent lost: %+v", tr.ToolResult)
	}
}

// TestPromptPayloadHasFrontendOverride covers the runtime guard that backs
// the "override fields MUST NOT be set by frontend" contract. A backend's
// handlePromptEvent calls this at entry to reject any non-empty result, so
// the table pins every field name + the empty case.
func TestPromptPayloadHasFrontendOverride(t *testing.T) {
	cases := []struct {
		name  string
		field string
		value string
	}{
		{"directory", "directory", "/etc"},
		{"modelSpec", "modelSpec", "sonnet"},
		{"agent", "agent", "code"},
		{"permission", "permission", "plan"},
		{"effort", "effort", "high"},
		{"settingsFile", "settingsFile", "/etc/foo.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &PromptPayload{ChatID: "c", Text: "hi"}
			setOverrideField(t, p, tc.field, tc.value)
			if got := p.HasFrontendOverride(); got != tc.field {
				t.Errorf("HasFrontendOverride = %q, want %q", got, tc.field)
			}
		})
	}
	t.Run("none", func(t *testing.T) {
		p := &PromptPayload{ChatID: "c", Text: "hi"}
		if got := p.HasFrontendOverride(); got != "" {
			t.Errorf("HasFrontendOverride = %q, want empty", got)
		}
	})
}

func setOverrideField(t *testing.T, p *PromptPayload, field, value string) {
	t.Helper()
	switch field {
	case "directory":
		p.Directory = value
	case "modelSpec":
		p.ModelSpec = value
	case "agent":
		p.Agent = value
	case "permission":
		p.Permission = value
	case "effort":
		p.Effort = value
	case "settingsFile":
		p.SettingsFile = value
	default:
		t.Fatalf("unknown override field %q", field)
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
