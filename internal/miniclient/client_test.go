package miniclient

import (
	"testing"
	"time"
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
}

func TestParseEvent_ResultFinish(t *testing.T) {
	// miniagent v1.1.0+ reports a finish reason on the result event; older
	// versions omit the field entirely (Finish stays empty).
	ev, _ := parseEvent([]byte(`{"type":"result","text":"","model":"kimi","finish":"max_iterations"}`))
	if ev.Finish != FinishMaxIterations {
		t.Errorf("Finish = %q, want %q", ev.Finish, FinishMaxIterations)
	}
	ev2, _ := parseEvent([]byte(`{"type":"result","text":"ok","finish":"stop"}`))
	if ev2.Finish != FinishStop {
		t.Errorf("Finish = %q, want %q", ev2.Finish, FinishStop)
	}
	// Absent finish (pre-v1.1.0 / non-result) parses without error.
	ev3, _ := parseEvent([]byte(`{"type":"result","text":"ok"}`))
	if ev3.Finish != "" {
		t.Errorf("Finish = %q, want empty when field absent", ev3.Finish)
	}
}

func TestParseEvent_Error(t *testing.T) {
	ev, _ := parseEvent([]byte(`{"type":"error","message":"boom"}`))
	if ev.Kind != KindError || ev.Message != "boom" {
		t.Errorf("got %+v", ev)
	}
	if !ev.IsTerminal {
		t.Error("error must be terminal")
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

// TestParseEvent_ToolResult covers the v2.0.0 tool_result event. A non-shell
// tool carries no exit_code (pointer stays nil); is_error propagates as-is.
// It is a non-terminal mid-stream event.
func TestParseEvent_ToolResult(t *testing.T) {
	ev, ok := parseEvent([]byte(`{"type":"tool_result","name":"read","call_id":"c1","output":"hi","truncated":false,"is_error":false}`))
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.Kind != KindToolResult {
		t.Errorf("Kind = %q, want tool_result", ev.Kind)
	}
	if ev.IsTerminal {
		t.Error("tool_result must not be terminal")
	}
	if ev.Name != "read" || ev.Output != "hi" {
		t.Errorf("got name=%q output=%q", ev.Name, ev.Output)
	}
	if ev.ExitCode != nil {
		t.Errorf("ExitCode = %v, want nil for non-shell tool", *ev.ExitCode)
	}
}

// TestParseEvent_ToolResult_ShellExitCode covers the v2.0.0 breaking change:
// shell non-zero exit reports exit_code and is_error=false (a legitimate
// command result, not an execution failure). The pointer distinguishes
// "absent" from a real code 0.
func TestParseEvent_ToolResult_ShellExitCode(t *testing.T) {
	ev, ok := parseEvent([]byte(`{"type":"tool_result","name":"shell","call_id":"c1","output":"err","is_error":false,"exit_code":1}`))
	if !ok {
		t.Fatal("expected ok")
	}
	if ev.IsError {
		t.Error("IsError = true, want false (v2: non-zero exit is not is_error)")
	}
	if ev.ExitCode == nil || *ev.ExitCode != 1 {
		t.Errorf("ExitCode = %v, want 1", ev.ExitCode)
	}
}

// TestParseEvent_Deltas covers the v2.0.0 streaming increments (text_delta /
// reasoning_delta), emitted only under -stream. Both reuse the text JSON key
// for the chunk and add a step index; neither is terminal.
func TestParseEvent_Deltas(t *testing.T) {
	td, ok := parseEvent([]byte(`{"type":"text_delta","step":2,"text":"foo"}`))
	if !ok {
		t.Fatal("expected ok")
	}
	if td.Kind != KindTextDelta || td.Step != 2 || td.Text != "foo" {
		t.Errorf("text_delta got %+v", td)
	}
	if td.IsTerminal {
		t.Error("text_delta must not be terminal")
	}
	rd, ok := parseEvent([]byte(`{"type":"reasoning_delta","step":1,"text":"think"}`))
	if !ok {
		t.Fatal("expected ok")
	}
	if rd.Kind != KindReasoningDelta || rd.Text != "think" {
		t.Errorf("reasoning_delta got %+v", rd)
	}
}

// TestParseEvent_UnknownType confirms a forward-compat guarantee: a type the
// bridge does not yet know still parses (ok=true), is non-terminal, and
// carries the raw type in Kind so the handler switch can ignore it.
func TestParseEvent_UnknownType(t *testing.T) {
	ev, ok := parseEvent([]byte(`{"type":"some_future_event","x":1}`))
	if !ok {
		t.Fatal("expected ok for unknown type (forward compat)")
	}
	if ev.IsTerminal {
		t.Error("unknown type must not be terminal")
	}
	if ev.Kind != "some_future_event" {
		t.Errorf("Kind = %q, want raw type passthrough", ev.Kind)
	}
}

func TestBuildArgs_Full(t *testing.T) {
	c := New(Config{
		CLIPath:      "/bin/miniagent",
		APIKey:       "sk-test",
		BaseURL:      "http://localhost:8080",
		SystemPrompt: "be brief",
		MaxTokens:    2048,
	}, nil)
	args := c.buildArgs(RunOptions{
		Prompt:  "hi",
		Model:   "kimi",
		Workdir: "/proj",
	})
	// Check the 5 surviving flags are present. -api-key is intentionally
	// absent: the CLI has no such flag, the key is passed via $MINIAGENT_API_KEY env.
	want := map[string]bool{
		"-model": false, "-base-url": false,
		"-system": false, "-max-tokens": false, "-workdir": false,
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
	// Only -model is guaranteed when others are empty. -api-key must NOT
	// appear (the CLI has no such flag; the key goes via env).
	hasFlag := func(f string) bool {
		for i, a := range args {
			if a == f && i+1 < len(args) {
				return true
			}
		}
		return false
	}
	if !hasFlag("-model") {
		t.Errorf("missing required flag -model: %v", args)
	}
	for _, a := range args {
		if a == "-api-key" {
			t.Errorf("-api-key must NOT be in args (CLI has no such flag): %v", args)
		}
	}
	if hasFlag("-workdir") {
		t.Errorf("workdir should be absent when empty: %v", args)
	}
}

// TestBuildArgs_NoRemovedFlags is a regression guard for the stateless
// migration: 5 of the 6 flags miniagent fe85c16 deleted (-verbose /
// -permission / -blocked-patterns / -chat-id / -state-dir) MUST NOT appear in
// buildArgs output — any would make Go's flag package os.Exit(2) at startup.
// (-stream was also deleted in fe85c16 but RE-ADDED in v2.0.0, so it is no
// longer banned; see TestBuildArgs_Stream.)
func TestBuildArgs_NoRemovedFlags(t *testing.T) {
	c := New(Config{
		CLIPath:      "/bin/ma",
		APIKey:       "k",
		BaseURL:      "http://x",
		SystemPrompt: "s",
		MaxTokens:    100,
	}, nil)
	args := c.buildArgs(RunOptions{Model: "m", Workdir: "/w"})
	banned := []string{"-verbose", "-permission", "-blocked-patterns", "-chat-id", "-state-dir"}
	for _, b := range banned {
		if contains(args, b) {
			t.Errorf("removed flag %q present in args: %v", b, args)
		}
	}
}

// TestBuildArgs_Stream verifies -stream is emitted only when configured (v2.0.0
// re-added the flag; it requires a v2.0.0+ binary, hence the opt-in).
func TestBuildArgs_Stream(t *testing.T) {
	off := New(Config{CLIPath: "/bin/ma", APIKey: "k"}, nil)
	if contains(off.buildArgs(RunOptions{Model: "m"}), "-stream") {
		t.Error("-stream present when Stream=false")
	}
	on := New(Config{CLIPath: "/bin/ma", APIKey: "k", Stream: true}, nil)
	if !contains(on.buildArgs(RunOptions{Model: "m"}), "-stream") {
		t.Error("-stream absent when Stream=true")
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

// argValue returns the value immediately following flag in args, or "" if the
// flag is absent or has no value (e.g. a bare bool flag at the tail).
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestBuildArgs_V2OptionalFlags verifies the v2.0.0 optional run flags are
// emitted with their values when configured. Zero/empty omission is covered by
// TestBuildArgs_Minimal's shape (these fields are absent from that Config).
func TestBuildArgs_V2OptionalFlags(t *testing.T) {
	c := New(Config{
		CLIPath:       "/bin/ma",
		APIKey:        "k",
		MaxIterations: 30,
		ShellTimeout:  90 * time.Second,
		Confine:       "workdir",
		KeyFile:       "/etc/miniagent/key",
	}, nil)
	args := c.buildArgs(RunOptions{Model: "m"})
	if v := argValue(args, "-max-iterations"); v != "30" {
		t.Errorf("-max-iterations = %q, want 30", v)
	}
	if v := argValue(args, "-shell-timeout"); v != "1m30s" {
		t.Errorf("-shell-timeout = %q, want 1m30s", v)
	}
	if v := argValue(args, "-confine"); v != "workdir" {
		t.Errorf("-confine = %q, want workdir", v)
	}
	if v := argValue(args, "-key-file"); v != "/etc/miniagent/key" {
		t.Errorf("-key-file = %q, want /etc/miniagent/key", v)
	}
}

// TestBuildArgs_V2OptionalFlags_Omitted confirms the v2.0.0 flags stay absent
// at zero values so a default config does not pass them (the CLI would still
// accept them, but the bridge should not invent settings the user did not set).
func TestBuildArgs_V2OptionalFlags_Omitted(t *testing.T) {
	c := New(Config{CLIPath: "/bin/ma", APIKey: "k"}, nil)
	args := c.buildArgs(RunOptions{Model: "m"})
	for _, f := range []string{"-max-iterations", "-shell-timeout", "-confine", "-key-file"} {
		if contains(args, f) {
			t.Errorf("zero-value flag %s should be omitted: %v", f, args)
		}
	}
}
