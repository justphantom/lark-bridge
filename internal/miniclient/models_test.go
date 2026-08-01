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

// TestListModels_OK verifies a -list-models mock printing one id per line is
// parsed into a clean slice (whitespace/blank lines trimmed).
func TestListModels_OK(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeModelsScript(t, t.TempDir(), "ok.sh",
		[]string{"gpt-4o", "gpt-4o-mini", "", "  deepseek-chat  "}, "", 0)
	c := New(Config{CLIPath: p, APIKey: "k", BaseURL: "http://x"}, nil)
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"gpt-4o", "gpt-4o-mini", "deepseek-chat"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("idx %d: got %q want %q", i, got[i], want[i])
		}
	}
}

// TestListModels_Empty verifies an endpoint returning nothing yields an empty
// slice without error (the caller surfaces a "list empty" hint).
func TestListModels_Empty(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	p := writeModelsScript(t, t.TempDir(), "empty.sh", nil, "", 0)
	c := New(Config{CLIPath: p, APIKey: "k"}, nil)
	got, err := c.ListModels(context.Background())
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
	_, err := c.ListModels(context.Background())
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
