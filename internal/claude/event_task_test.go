//go:build linux || darwin

package claude

import (
	"testing"
)

func TestParseEvent_TaskStarted(t *testing.T) {
	line := `{"type":"system","subtype":"task_started","task_id":"t1","tool_use_id":"tu_1","description":"Explore codebase architecture","subagent_type":"Explore","task_type":"local_agent","prompt":"...","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventTaskStarted {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.TaskType != "Explore" {
		t.Errorf("TaskType = %q, want Explore", ev.TaskType)
	}
	if ev.TaskDesc != "Explore codebase architecture" {
		t.Errorf("TaskDesc = %q", ev.TaskDesc)
	}
	if ev.TaskID != "t1" {
		t.Errorf("TaskID = %q, want t1", ev.TaskID)
	}
}

func TestParseEvent_TaskProgress(t *testing.T) {
	// task_progress carries a live description (changes per tick) plus
	// cumulative usage. These fields drive the subagent progress updates.
	line := `{"type":"system","subtype":"task_progress","task_id":"t1","tool_use_id":"tu_1","description":"Reading internal/opencode/model.go","subagent_type":"Explore","usage":{"total_tokens":104609,"tool_uses":65,"duration_ms":59675},"last_tool_name":"Read","session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventTaskProgress {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.TaskDesc != "Reading internal/opencode/model.go" {
		t.Errorf("TaskDesc = %q", ev.TaskDesc)
	}
	if ev.TaskTokens != 104609 {
		t.Errorf("TaskTokens = %d, want 104609", ev.TaskTokens)
	}
	if ev.TaskSteps != 65 {
		t.Errorf("TaskSteps = %d, want 65", ev.TaskSteps)
	}
	if ev.TaskMs != 59675 {
		t.Errorf("TaskMs = %d, want 59675", ev.TaskMs)
	}
}

func TestParseEvent_TaskNotification(t *testing.T) {
	// task_notification carries the terminal summary (not description) and
	// marks non-completed status as an error so the caller can flag the row.
	line := `{"type":"system","subtype":"task_notification","task_id":"t1","tool_use_id":"tu_1","status":"completed","output_file":"","summary":"Explore codebase architecture","usage":{"total_tokens":107296,"tool_uses":66,"duration_ms":98342},"session_id":"s1"}`
	got, err := parseEvent(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 || got[0].Type != EventTaskNotification {
		t.Fatalf("got %+v", got)
	}
	ev := got[0]
	if ev.TaskDesc != "Explore codebase architecture" {
		t.Errorf("TaskDesc = %q (should come from summary)", ev.TaskDesc)
	}
	if ev.IsToolError {
		t.Errorf("completed status should not be an error")
	}
	if ev.TaskSteps != 66 || ev.TaskMs != 98342 {
		t.Errorf("usage = steps %d ms %d", ev.TaskSteps, ev.TaskMs)
	}
}

func TestParseEvent_TaskNotificationFailed(t *testing.T) {
	line := `{"type":"system","subtype":"task_notification","task_id":"t1","status":"failed","summary":"boom","usage":{"total_tokens":10,"tool_uses":1,"duration_ms":100},"session_id":"s1"}`
	got, _ := parseEvent(line)
	if !got[0].IsToolError {
		t.Errorf("non-completed status should flag IsToolError")
	}
}
