package miniclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeModelsScript writes a bash mock that prints the given stdout lines (one
// per line), optionally writes stderrMsg to stderr, then exits with code. Used
// to drive ListModels without the real miniagent binary.
func writeModelsScript(t *testing.T, dir, name string, stdoutLines []string, stderrMsg string, exitCode int) string {
	t.Helper()
	p := filepath.Join(dir, name)
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	if stderrMsg != "" {
		sb.WriteString("printf '%s\\n' " + quoteForSh(stderrMsg) + " >&2\n")
	}
	for _, l := range stdoutLines {
		sb.WriteString("printf '%s\\n' " + quoteForSh(l) + "\n")
	}
	sb.WriteString("exit " + itoa(exitCode) + "\n")
	if err := os.WriteFile(p, []byte(sb.String()), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

// modelEvent encodes one -list-models NDJSON line for test fixtures.
func modelEvent(provider, model string) string {
	return `{"type":"model","provider":"` + provider + `","model":"` + model + `"}`
}

// assertRefs checks got matches the (provider,model) pairs in want, in order.
func assertRefs(t *testing.T, got []ModelRef, want []ModelRef) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d refs %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

// TestListModels_OK verifies a -list-models mock emitting one NDJSON model
// event per line is parsed into ModelRef pairs, skipping blank lines, non-JSON
// lines (stderr bleed-through), and non-model events.
func TestListModels_OK(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeModelsScript(t, t.TempDir(), "ok.sh",
		[]string{
			modelEvent("p", "gpt-4o"),
			modelEvent("p", "gpt-4o-mini"),
			"",                                  // blank line
			"  " + modelEvent("p", "deepseek-chat") + "  ", // surrounding whitespace
			"not-json-stderr-bleed",             // skipped (not JSON)
			`{"type":"result","text":"x"}`,      // skipped (type != model)
		}, "", 0)
	c := New(Config{CLIPath: p, APIKey: "k", ConfigPath: "/etc/miniagent/miniagent.json"}, nil)
	got, err := c.ListModels(context.Background(), "")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	assertRefs(t, got, []ModelRef{
		{Provider: "p", Model: "gpt-4o"},
		{Provider: "p", Model: "gpt-4o-mini"},
		{Provider: "p", Model: "deepseek-chat"},
	})
}

// TestListModels_MultiProvider verifies the provider field is carried per line
// (aggregating models from multiple providers), so the picker can show and the
// Run path can replay the right provider/model pair.
func TestListModels_MultiProvider(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeModelsScript(t, t.TempDir(), "multi.sh",
		[]string{
			modelEvent("openai", "gpt-4o"),
			modelEvent("deepseek", "deepseek-chat"),
		}, "", 0)
	c := New(Config{CLIPath: p, APIKey: "k"}, nil)
	got, err := c.ListModels(context.Background(), "")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	assertRefs(t, got, []ModelRef{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "deepseek", Model: "deepseek-chat"},
	})
}

// TestListModels_Empty verifies an endpoint returning nothing yields an empty
// slice without error (the caller surfaces a "list empty" hint).
func TestListModels_Empty(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeModelsScript(t, t.TempDir(), "empty.sh", nil, "", 0)
	c := New(Config{CLIPath: p, APIKey: "k"}, nil)
	got, err := c.ListModels(context.Background(), "")
	if err != nil {
		t.Fatalf("ListModels on empty: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

// TestListModels_NonZeroExit verifies a failing fork (the CLI exits 1 — e.g. a
// 404 / not-implemented endpoint, or an older binary that lacks -list-models)
// surfaces an error carrying the stderr diagnostic and the /model fallback hint.
func TestListModels_NonZeroExit(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeModelsScript(t, t.TempDir(), "fail.sh", nil, "endpoint returned 404", 1)
	c := New(Config{CLIPath: p, APIKey: "k"}, nil)
	_, err := c.ListModels(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %q, want it to include stderr diagnostic", err.Error())
	}
	if !strings.Contains(err.Error(), "/model") {
		t.Errorf("err = %q, want it to mention the /model <id> fallback", err.Error())
	}
}

// writeArgsEchoScript writes a bash mock that echoes its own argv back as one
// NDJSON model event per token (model = the token). Used to verify ListModels
// builds the expected -config flag without the real miniagent binary: the argv
// tokens resurface as the Model field of the parsed refs.
func writeArgsEchoScript(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	body := "#!/bin/bash\nfor a in \"$@\"; do printf '{\"type\":\"model\",\"provider\":\"p\",\"model\":\"%s\"}\\n' \"$a\"; done\nexit 0\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

// modelsAsArgs joins the Model field of each ref (the echoed argv tokens) into
// one space-separated string for substring assertions.
func modelsAsArgs(refs []ModelRef) string {
	parts := make([]string, len(refs))
	for i, r := range refs {
		parts[i] = r.Model
	}
	return strings.Join(parts, " ")
}

// TestListModels_ConfigModeArgs verifies v3.1+ config-only mode: ListModels
// passes -config <abspath> and does NOT pass the removed -chat-url / -models-url.
func TestListModels_ConfigModeArgs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeArgsEchoScript(t, t.TempDir(), "echo.sh")
	c := New(Config{
		CLIPath:    p,
		APIKey:     "k",
		ConfigPath: "/etc/miniagent/miniagent.json",
	}, nil)
	got, err := c.ListModels(context.Background(), "")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	joined := modelsAsArgs(got)
	if !strings.Contains(joined, "-config") || !strings.Contains(joined, "/etc/miniagent/miniagent.json") {
		t.Errorf("config mode args missing -config <path>; got %v", got)
	}
	if strings.Contains(joined, "-chat-url") {
		t.Errorf("-chat-url must NOT appear (removed in v3.1): %v", got)
	}
	if strings.Contains(joined, "-models-url") {
		t.Errorf("-models-url must NOT appear (removed in v3.1): %v", got)
	}
}

// TestListModels_ConfigModeArgs_Override verifies the per-chat -config override
// on the -list-models fork: a non-empty configPath (the chat's /config pin)
// replaces the client's startup ConfigPath, so /model and /models list models
// for the chat's currently pinned config rather than the global default.
func TestListModels_ConfigModeArgs_Override(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeArgsEchoScript(t, t.TempDir(), "echo.sh")
	c := New(Config{
		CLIPath:    p,
		APIKey:     "k",
		ConfigPath: "/etc/miniagent/miniagent.json", // startup default
	}, nil)
	got, err := c.ListModels(context.Background(), "/home/u/.miniagent/kimi-miniagent.json")
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	joined := modelsAsArgs(got)
	if !strings.Contains(joined, "-config /home/u/.miniagent/kimi-miniagent.json") {
		t.Errorf("override: -config should be the kimi path; got %v", got)
	}
	if strings.Contains(joined, "/etc/miniagent/miniagent.json") {
		t.Errorf("override: startup default must NOT appear; got %v", got)
	}
}
