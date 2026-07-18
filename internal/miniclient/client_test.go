package miniclient

import (
	"testing"
)

func TestParseEvent_ToolUse(t *testing.T) {
	ev, ok := parseEvent([]byte(`{"type":"tool_use","name":"read_file","input":"{\"path\":\"x\"}"}`))
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.Kind != KindToolUse || ev.Name != "read_file" {
		t.Errorf("got %+v", ev)
	}
	if ev.IsTerminal {
		t.Error("tool_use must not be terminal")
	}
}

func TestParseEvent_ToolResult(t *testing.T) {
	ev, _ := parseEvent([]byte(`{"type":"tool_result","name":"shell","output":"ok","is_error":false}`))
	if ev.Kind != KindToolResult || ev.Output != "ok" || ev.IsError {
		t.Errorf("got %+v", ev)
	}
}

func TestParseEvent_Result(t *testing.T) {
	ev, _ := parseEvent([]byte(`{"type":"result","text":"hello","model":"kimi","input_tokens":10,"output_tokens":5,"steps":1}`))
	if ev.Kind != KindResult || ev.Text != "hello" {
		t.Errorf("got %+v", ev)
	}
	if !ev.IsTerminal {
		t.Error("result must be terminal")
	}
	if ev.InputTokens != 10 || ev.OutputTokens != 5 || ev.Steps != 1 {
		t.Errorf("usage = in=%d out=%d steps=%d", ev.InputTokens, ev.OutputTokens, ev.Steps)
	}
	if ev.Incomplete {
		t.Error("absent incomplete must decode as false")
	}
}

// TestParseEvent_Incomplete locks the contract that a result event with
// incomplete=true (miniagent hit its iteration cap) is decoded faithfully.
// Without this the bridge silently turns "ran out of steps" into "empty
// reply" — the user sees a blank card with no explanation.
func TestParseEvent_Incomplete(t *testing.T) {
	ev, _ := parseEvent([]byte(`{"type":"result","model":"kimi","input_tokens":8200,"output_tokens":1500,"steps":20,"incomplete":true}`))
	if ev.Kind != KindResult {
		t.Fatalf("kind = %q, want result", ev.Kind)
	}
	if !ev.Incomplete {
		t.Error("incomplete must be true when the field is present and true")
	}
	if ev.Text != "" {
		t.Errorf("text = %q, want empty (truncated turns emit no text)", ev.Text)
	}
	if ev.Steps != 20 {
		t.Errorf("steps = %d, want 20", ev.Steps)
	}
}

func TestParseEvent_Error(t *testing.T) {
	ev, _ := parseEvent([]byte(`{"type":"error","message":"boom"}`))
	if ev.Kind != KindError || ev.Message != "boom" {
		t.Errorf("got %+v", ev)
	}
	if !ev.IsTerminal || !ev.IsError {
		t.Error("error must be terminal + isError")
	}
}

func TestParseEvent_Malformed(t *testing.T) {
	_, ok := parseEvent([]byte(`not json`))
	if ok {
		t.Error("expected ok=false for malformed JSON")
	}
}

func TestParseEvent_EmptyType(t *testing.T) {
	ev, ok := parseEvent([]byte(`{"type":""}`))
	if !ok {
		t.Fatal("expected ok (valid JSON)")
	}
	if ev.IsTerminal {
		t.Error("empty type must not be terminal")
	}
}

func TestBuildArgs_Full(t *testing.T) {
	c := New(Config{
		CLIPath:      "/bin/miniagent",
		APIKey:       "sk-test",
		BaseURL:      "http://localhost:8080",
		SystemPrompt: "be brief",
		MaxTokens:    2048,
		Permission:   "free",
	}, nil)
	args := c.buildArgs(RunOptions{
		Prompt:   "hi",
		Model:    "kimi",
		Workdir:  "/proj",
		ChatID:   "c1",
		StateDir: "/tmp/ma",
	})
	// Check key flags are present. --api-key is intentionally absent: the
	// CLI has no such flag, the key is passed via $MINIAGENT_API_KEY env.
	want := map[string]bool{
		"--model": false, "--base-url": false,
		"--system": false, "--max-tokens": false, "--permission": false,
		"--verbose": false, "--workdir": false, "--chat-id": false,
		"--state-dir": false,
	}
	for _, a := range args {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for flag, found := range want {
		if !found {
			t.Errorf("missing flag %s in buildArgs output: %v", flag, args)
		}
	}
}

func TestBuildArgs_Minimal(t *testing.T) {
	c := New(Config{CLIPath: "/bin/ma", APIKey: "k"}, nil)
	args := c.buildArgs(RunOptions{Model: "m"})
	// Only model + verbose are guaranteed when others are empty. --api-key
	// must NOT appear (the CLI has no such flag; the key goes via env).
	hasFlag := func(f string) bool {
		for i, a := range args {
			if a == f && i+1 < len(args) {
				return true
			}
		}
		return false
	}
	if !hasFlag("--model") {
		t.Errorf("missing required flag --model: %v", args)
	}
	for _, a := range args {
		if a == "--api-key" {
			t.Errorf("--api-key must NOT be in args (CLI has no such flag): %v", args)
		}
	}
	if !contains(args, "--verbose") {
		t.Errorf("verbose should always be present: %v", args)
	}
	if hasFlag("--workdir") {
		t.Errorf("workdir should be absent when empty: %v", args)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
