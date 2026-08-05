package miniclient

import (
	"context"
	"os"
	"path/filepath"
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
		SystemPrompt: "be brief",
		MaxTokens:    2048,
		ConfigPath:   "/etc/miniagent/miniagent.json",
	}, nil)
	args := c.buildArgs(RunOptions{
		Prompt:  "hi",
		Model:   "kimi",
		Workdir: "/proj",
	})
	// Check the surviving flags are present. -api-key is intentionally absent:
	// the CLI has no such flag, the key is passed via $MINIAGENT_API_KEY env.
	// -config is always emitted (v3.1+ config-only mode).
	want := map[string]bool{
		"-model": false, "-config": false,
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
	// v3 removed -base-url/-confine; v3.1 removed -chat-url/-models-url/
	// -context-window/-shell-timeout: assert they stay out (regression guard).
	for _, b := range []string{"-base-url", "-confine", "-chat-url", "-models-url", "-context-window", "-shell-timeout"} {
		if contains(args, b) {
			t.Errorf("removed flag present in args: %v", args)
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
// v3.0.0 additionally removed -base-url/-confine; v3.1 removed -chat-url/
// -models-url/-context-window/-shell-timeout (config-only mode), so all are banned.
// (-stream was also deleted in fe85c16 but RE-ADDED in v2.0.0, so it is no
// longer banned; see TestBuildArgs_Stream.)
func TestBuildArgs_NoRemovedFlags(t *testing.T) {
	c := New(Config{
		CLIPath:      "/bin/ma",
		APIKey:       "k",
		SystemPrompt: "s",
		MaxTokens:    100,
		ConfigPath:   "/etc/miniagent/miniagent.json",
	}, nil)
	args := c.buildArgs(RunOptions{Model: "m", Workdir: "/w"})
	banned := []string{
		"-verbose", "-permission", "-blocked-patterns", "-chat-id", "-state-dir",
		"-base-url", "-confine",
		"-chat-url", "-models-url", "-context-window", "-shell-timeout",
	}
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

// TestBuildArgs_V2OptionalFlags verifies the optional run flags still exposed
// as CLI flags are emitted with their values when configured. Zero/empty
// omission is covered by TestBuildArgs_Minimal's shape (these fields are
// absent from that Config). (-shell-timeout moved to miniagent.json in v3.1.)
// KeyFile is intentionally NOT a flag: miniagent removed -key-file post-3.4.0,
// so even when KeyFile is configured it must stay out of buildArgs (the bridge
// reads the file and injects the key via env; see TestEffectiveAPIKey).
func TestBuildArgs_V2OptionalFlags(t *testing.T) {
	c := New(Config{
		CLIPath:       "/bin/ma",
		APIKey:        "k",
		MaxIterations: 30,
		KeyFile:       "/etc/miniagent/key",
		ConfigPath:    "/etc/miniagent/miniagent.json",
	}, nil)
	args := c.buildArgs(RunOptions{Model: "m"})
	if v := argValue(args, "-max-iterations"); v != "30" {
		t.Errorf("-max-iterations = %q, want 30", v)
	}
	if contains(args, "-key-file") {
		t.Errorf("-key-file must NOT appear (removed upstream): %v", args)
	}
}

// TestBuildArgs_V2OptionalFlags_Omitted confirms the optional flags stay
// absent at zero values so a default config does not pass them (the CLI would
// still accept them, but the bridge should not invent settings the user did
// not set). -config is intentionally NOT in this list: it is always emitted
// in v3.1+ config-only mode.
func TestBuildArgs_V2OptionalFlags_Omitted(t *testing.T) {
	c := New(Config{CLIPath: "/bin/ma", APIKey: "k"}, nil)
	args := c.buildArgs(RunOptions{Model: "m"})
	for _, f := range []string{
		"-max-iterations", "-key-file",
		"-mode", "-thinking",
		"-session",
	} {
		if contains(args, f) {
			t.Errorf("zero-value flag %s should be omitted: %v", f, args)
		}
	}
}

// TestBuildArgs_ConfigPath verifies v3 config mode: when ConfigPath is
// non-empty, -config <abspath> is emitted and -chat-url/-models-url are NOT
// (endpoints come from miniagent.json). -model and other flags still appear.
//
// This also pins the Phase 4 contract: config mode is NOT a "minimal args"
// mode — it only swaps the endpoint source. The per-turn -mode/-thinking and
// the per-chat -session MUST still appear alongside -config so that /mode,
// /thinking, and per-chat memory keep working under multi-provider deployments.
func TestBuildArgs_ConfigPath(t *testing.T) {
	c := New(Config{
		CLIPath:    "/bin/ma",
		APIKey:     "k",
		ConfigPath: "/etc/miniagent/miniagent.json",
		Mode:       "default", // client default still emits -mode in config mode
		Thinking:   "off",     // client default still emits -thinking in config mode
	}, nil)
	args := c.buildArgs(RunOptions{
		Model:   "main/gpt-4o",
		Workdir: "/w",
		Session: "/var/lib/lark-bridge/miniagent-sessions/abc.jsonl",
	})
	if v := argValue(args, "-config"); v != "/etc/miniagent/miniagent.json" {
		t.Errorf("-config = %q, want miniagent.json path", v)
	}
	if contains(args, "-chat-url") {
		t.Errorf("-chat-url must NOT appear in config mode: %v", args)
	}
	if contains(args, "-models-url") {
		t.Errorf("-models-url must NOT appear in config mode: %v", args)
	}
	if v := argValue(args, "-model"); v != "main/gpt-4o" {
		t.Errorf("-model = %q, want main/gpt-4o", v)
	}
	if v := argValue(args, "-workdir"); v != "/w" {
		t.Errorf("-workdir = %q, want /w", v)
	}
	// Phase 4: -mode/-thinking/-session co-exist with -config (config mode only
	// re-routes endpoints, not the per-chat turn shape).
	if v := argValue(args, "-mode"); v != "default" {
		t.Errorf("-mode = %q, want default (config mode keeps per-turn mode)", v)
	}
	if v := argValue(args, "-thinking"); v != "off" {
		t.Errorf("-thinking = %q, want off (config mode keeps per-turn thinking)", v)
	}
	if v := argValue(args, "-session"); v != "/var/lib/lark-bridge/miniagent-sessions/abc.jsonl" {
		t.Errorf("-session = %q, want the per-chat jsonl path", v)
	}
}

// TestBuildArgs_ConfigPathOverride verifies the per-chat -config override
// (set by miniagent /config): RunOptions.ConfigPath wins over the client's
// startup ConfigPath; empty falls back to the startup default so existing
// turns keep their pre-/config behaviour.
func TestBuildArgs_ConfigPathOverride(t *testing.T) {
	c := New(Config{
		CLIPath:    "/bin/ma",
		APIKey:     "k",
		ConfigPath: "/etc/miniagent/miniagent.json", // startup default
	}, nil)
	// Per-turn override wins.
	args := c.buildArgs(RunOptions{
		Model:      "m",
		ConfigPath: "/home/u/.miniagent/kimi-miniagent.json",
	})
	if v := argValue(args, "-config"); v != "/home/u/.miniagent/kimi-miniagent.json" {
		t.Errorf("override: -config = %q, want kimi path", v)
	}
	// Empty override falls back to the startup default.
	args = c.buildArgs(RunOptions{Model: "m"})
	if v := argValue(args, "-config"); v != "/etc/miniagent/miniagent.json" {
		t.Errorf("fallback: -config = %q, want startup default", v)
	}
}

// TestBuildArgs_V3ModeThinking verifies the v3 -mode/-thinking flags appear
// with their configured values when set on the client (the per-chat "" path:
// RunOptions.Mode/Thinking empty → client default). (-context-window moved to
// miniagent.json in v3.1.)
func TestBuildArgs_V3ModeThinking(t *testing.T) {
	c := New(Config{
		CLIPath:  "/bin/ma",
		APIKey:   "k",
		Mode:     "auto",
		Thinking: "high",
	}, nil)
	args := c.buildArgs(RunOptions{Model: "m", Workdir: "/w"})
	if v := argValue(args, "-mode"); v != "auto" {
		t.Errorf("-mode = %q, want auto", v)
	}
	if v := argValue(args, "-thinking"); v != "high" {
		t.Errorf("-thinking = %q, want high", v)
	}
}

// TestBuildArgs_PerTurnModeThinkingOverride verifies a non-empty
// RunOptions.Mode/Thinking overrides the client's configured default — this is
// the per-chat pin path (handler.activeTurnConfig → binding.Mode/Thinking).
func TestBuildArgs_PerTurnModeThinkingOverride(t *testing.T) {
	c := New(Config{
		CLIPath:  "/bin/ma",
		APIKey:   "k",
		Mode:     "default", // global default
		Thinking: "off",     // global default
	}, nil)
	args := c.buildArgs(RunOptions{
		Model:    "m",
		Workdir:  "/w",
		Mode:     "auto", // per-chat pin wins
		Thinking: "max",  // per-chat pin wins
	})
	if v := argValue(args, "-mode"); v != "auto" {
		t.Errorf("-mode = %q, want per-turn override auto", v)
	}
	if v := argValue(args, "-thinking"); v != "max" {
		t.Errorf("-thinking = %q, want per-turn override max", v)
	}
}

// TestBuildArgs_PerTurnMaxIterOverride verifies a >0 RunOptions.MaxIterations
// overrides the client's configured default — the per-chat pin path
// (handler.activeMaxIter → binding.MaxIterations). A per-turn 0 does NOT
// override: 0 is the "unset" sentinel, so it falls back to the client default.
func TestBuildArgs_PerTurnMaxIterOverride(t *testing.T) {
	c := New(Config{
		CLIPath:       "/bin/ma",
		APIKey:        "k",
		MaxIterations: 30, // global default
	}, nil)
	// Per-chat pin wins over the global default.
	args := c.buildArgs(RunOptions{Model: "m", MaxIterations: 50})
	if v := argValue(args, "-max-iterations"); v != "50" {
		t.Errorf("-max-iterations = %q, want per-turn override 50", v)
	}
	// Per-turn 0 (= unset) falls back to the client default of 30.
	args = c.buildArgs(RunOptions{Model: "m"})
	if v := argValue(args, "-max-iterations"); v != "30" {
		t.Errorf("-max-iterations = %q, want client default 30 when per-turn is 0", v)
	}
}

// TestDefaultMaxIterations verifies the accessor surfaces the configured
// -max-iterations default (0 when unset), which the miniagent handler uses for
// /maxiter display.
func TestDefaultMaxIterations(t *testing.T) {
	unset := New(Config{CLIPath: "/bin/ma", APIKey: "k"}, nil)
	if got := unset.DefaultMaxIterations(); got != 0 {
		t.Errorf("unset DefaultMaxIterations = %d, want 0", got)
	}
	set := New(Config{CLIPath: "/bin/ma", APIKey: "k", MaxIterations: 25}, nil)
	if got := set.DefaultMaxIterations(); got != 25 {
		t.Errorf("DefaultMaxIterations = %d, want 25", got)
	}
}

// TestIsReady_MissingBinary fails fast when the CLI is absent: IsReady returns
// an error rather than silently proceeding. This is the startup health gate
// tested here; happy-path version checks are implicit in the v3.3.0 gate.
func TestIsReady_MissingBinary(t *testing.T) {
	c := New(Config{CLIPath: "/nonexistent/miniagent-binary", APIKey: "k"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := c.IsReady(ctx)
	if err == nil {
		t.Fatal("IsReady with missing CLI should return an error")
	}
}

// TestCompareVersion pins the component-wise numeric comparison: lexicographic
// would mis-order 3.10.0 < 3.2.0 (string "10" < "2"), so the bridge must
// compare integer components. Also covers the shorter-version padding
// (3.3 == 3.3.0) and the pre-release suffix strip (3.3.0-rc1 == 3.3.0).
func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"3.3.0", "3.3.0", 0},
		{"3.10.0", "3.2.0", 1},  // NOT lexicographic (10 > 2, not "10" < "2")
		{"3.2.0", "3.10.0", -1}, // symmetric
		{"3.3.0", "3.3", 0},     // shorter pads missing components as 0
		{"3.3", "3.3.1", -1},
		{"3.3.0-rc1", "3.3.0", 0}, // pre-release suffix stripped
		{"3.3.1", "3.3.0", 1},
		{"3.0.0", "3.3.0", -1},
		{"4.0.0", "3.3.0", 1},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Errorf("compareVersion(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestSatisfiesVersion pins the version-gate logic the startup health check
// (IsReady) uses against minSupportedVersion. "dev" (untagged local build)
// always passes so local development is not blocked. A pre-release of the
// minimum (3.5.0-rc1) is treated as the release version and passes.
func TestSatisfiesVersion(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"dev", true}, // untagged local build — always pass
		{"3.5.0", true},
		{"3.5.1", true},
		{"3.10.0", true},
		{"4.0.0", true},
		{"3.5.0-rc1", true}, // pre-release strips to 3.5.0
		{"3.5", true},       // == 3.5.0
		{"3.4.0", false},    // below 3.5.0
		{"3.3.0", false},    // the previous floor — now rejected
		{"3.0.0", false},
		{"2.0.0", false},
	}
	for _, c := range cases {
		if got := satisfiesVersion(c.v, minSupportedVersion); got != c.want {
			t.Errorf("satisfiesVersion(%q, %q) = %v, want %v", c.v, minSupportedVersion, got, c.want)
		}
	}
}

// TestEffectiveAPIKey pins the post-3.4.0 KeyFile semantics: miniagent removed
// -key-file, so the bridge resolves the key itself. KeyFile takes precedence
// over APIKey when set (its contents, trimmed), and a missing key_file errors.
func TestEffectiveAPIKey(t *testing.T) {
	// APIKey path (no KeyFile).
	c := New(Config{CLIPath: "/bin/ma", APIKey: "sk-inline"}, nil)
	if k, err := c.effectiveAPIKey(); err != nil || k != "sk-inline" {
		t.Fatalf("no KeyFile: got %q err=%v, want sk-inline", k, err)
	}

	// KeyFile takes precedence over APIKey and is trimmed.
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("  sk-from-file\n"), 0o600); err != nil {
		t.Fatalf("write keyfile: %v", err)
	}
	c2 := New(Config{CLIPath: "/bin/ma", APIKey: "sk-inline", KeyFile: keyFile}, nil)
	if k, err := c2.effectiveAPIKey(); err != nil || k != "sk-from-file" {
		t.Fatalf("KeyFile: got %q err=%v, want sk-from-file", k, err)
	}

	// Missing key_file errors (so Run/ListModels fail fast, not at the CLI).
	c3 := New(Config{CLIPath: "/bin/ma", KeyFile: filepath.Join(dir, "nope")}, nil)
	if _, err := c3.effectiveAPIKey(); err == nil {
		t.Fatal("missing key_file should error")
	}
}
