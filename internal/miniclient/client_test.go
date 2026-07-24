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
// migration: the 6 flags miniagent fe85c16 deleted (-verbose / -stream /
// -permission / -blocked-patterns / -chat-id / -state-dir) MUST NOT appear
// in buildArgs output. Any of them would make Go's flag package os.Exit(2)
// at startup.
func TestBuildArgs_NoRemovedFlags(t *testing.T) {
	c := New(Config{
		CLIPath:      "/bin/ma",
		APIKey:       "k",
		BaseURL:      "http://x",
		SystemPrompt: "s",
		MaxTokens:    100,
	}, nil)
	args := c.buildArgs(RunOptions{Model: "m", Workdir: "/w"})
	banned := []string{"-verbose", "-stream", "-permission", "-blocked-patterns", "-chat-id", "-state-dir"}
	for _, b := range banned {
		if contains(args, b) {
			t.Errorf("removed flag %q present in args: %v", b, args)
		}
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
