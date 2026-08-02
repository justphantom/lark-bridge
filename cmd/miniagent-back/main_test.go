package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRun_BadConfigReturnsError pins the fail-fast contract: a missing
// config path makes run() return an error (which main promotes to
// os.Exit(1)) rather than crashing or silently proceeding. The error path
// is the only part of main worth pinning without forking a real miniagent
// subprocess; happy paths are covered by internal/miniagent tests.
func TestRun_BadConfigReturnsError(t *testing.T) {
	if err := run("/nonexistent/lark-miniagent-back-config.json"); err == nil {
		t.Fatal("run with missing config should return an error")
	}
}

// writeMiniAgentConfig writes a JSON config that loads cleanly and clears the
// cross-binary validators + the APIKey/Model requireds in run(), so the
// WorkspaceRoot / ConfigPath gates are the FIRST ones reachable. The
// caller passes the miniagent fields it wants to test (the helper fills the
// rest with valid values). state_dir points at a pre-created temp dir so the
// validate() writability check passes; returns the config path.
//
// We test run() end-to-end up to the validation gate rather than factoring
// the gate into its own function — per the v3 implementation manual, product
// code must not be reshaped just for tests.
func writeMiniAgentConfig(t *testing.T, miniagentJSON string) string {
	t.Helper()
	stateDir := t.TempDir()
	cfg := `{
  "ipc_secret":   "test-secret",
  "backend_id":   "miniagent-test",
  "frontend_url": "https://frontend.example.test:6060",
  "state_dir":    "` + stateDir + `",
  "miniagent": ` + miniagentJSON + `
}`
	p := filepath.Join(t.TempDir(), "miniagent-config.json")
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

// TestRun_WorkspaceRootRequired pins the v3 Phase 1 startup gate: an empty
// miniagent.workspace_root makes run() fail fast with a clear message. v3
// -mode default requires a workdir (it bounds the write tools + /cd picker);
// letting it through would surface as an opaque exit-1 on the first prompt.
func TestRun_WorkspaceRootRequired(t *testing.T) {
	// workspace_root omitted (empty). api_key/model set so the gates BEFORE
	// workspace_root pass; workspace_root is the first one to fire.
	p := writeMiniAgentConfig(t, `{
		"api_key":   "sk-test",
		"model":     "kimi"
	}`)
	err := run(p)
	if err == nil {
		t.Fatal("run with empty workspace_root should return an error")
	}
	if !strings.Contains(err.Error(), "workspace_root") {
		t.Errorf("err = %q, want it to mention workspace_root", err.Error())
	}
}

// TestRun_ConfigPathRequired pins the v3.1+ config-only gate: an empty
// miniagent.config_path makes run() fail fast. v3.1 removed bare CLI mode
// (-chat-url/-models-url), so the endpoint must come from config_path →
// miniagent.json.
func TestRun_ConfigPathRequired(t *testing.T) {
	// api_key/model/workspace_root set; config_path empty.
	p := writeMiniAgentConfig(t, `{
		"api_key":        "sk-test",
		"model":          "kimi",
		"workspace_root": "/tmp/miniagent-ws"
	}`)
	err := run(p)
	if err == nil {
		t.Fatal("run with empty config_path should return an error")
	}
	if !strings.Contains(err.Error(), "config_path") {
		t.Errorf("err = %q, want it to mention config_path", err.Error())
	}
	if strings.Contains(err.Error(), "bare mode") || strings.Contains(err.Error(), "chat_url") {
		t.Errorf("err = %q must not reference removed bare-mode/chat_url", err.Error())
	}
}

// TestRun_ConfigPathSatisfiesGate pins the v3.1+ config-only path: a non-empty
// config_path satisfies the startup gate. run() proceeds past the gate (it
// will still fail later — no real frontend, no miniagent binary — but the
// FAILURE MUST NOT be the config_path-required gate). This is the
// negative-space assertion for TestRun_ConfigPathRequired.
func TestRun_ConfigPathSatisfiesGate(t *testing.T) {
	p := writeMiniAgentConfig(t, `{
		"api_key":        "sk-test",
		"model":          "main/kimi",
		"workspace_root": "/tmp/miniagent-ws",
		"config_path":    "/etc/miniagent/miniagent.json"
	}`)
	err := run(p)
	if err == nil {
		t.Fatal("run should still return an error (no real frontend / binary)")
	}
	if strings.Contains(err.Error(), "config_path is required") {
		t.Errorf("config_path set must satisfy the gate; err = %q", err.Error())
	}
}

// TestRun_ConfigPathRelativeRejected pins the absolute-path gate: a relative
// config_path is rejected before the backend registers with the frontend.
// This prevents the operator from accidentally pointing at a cwd-relative
// path and polluting a non-obvious directory.
func TestRun_ConfigPathRelativeRejected(t *testing.T) {
	p := writeMiniAgentConfig(t, `{
		"api_key":        "sk-test",
		"model":          "main/kimi",
		"workspace_root": "/tmp/miniagent-ws",
		"config_path":    "relative/miniagent.json"
	}`)
	err := run(p)
	if err == nil {
		t.Fatal("run with relative config_path should return an error")
	}
	if !strings.Contains(err.Error(), "absolute") && !strings.Contains(err.Error(), "config_path") {
		t.Errorf("err = %q, want it to mention absolute path or config_path", err)
	}
}
