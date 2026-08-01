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
// WorkspaceRoot / ChatURL-ConfigPath gates are the FIRST ones reachable. The
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
	// workspace_root omitted (empty). api_key/model/chat_url set so the gates
	// BEFORE workspace_root pass; workspace_root is the first one to fire.
	p := writeMiniAgentConfig(t, `{
		"api_key":   "sk-test",
		"model":     "kimi",
		"chat_url":  "https://ex.test/v1/chat/completions"
	}`)
	err := run(p)
	if err == nil {
		t.Fatal("run with empty workspace_root should return an error")
	}
	if !strings.Contains(err.Error(), "workspace_root") {
		t.Errorf("err = %q, want it to mention workspace_root", err.Error())
	}
}

// TestRun_BareModeRequiresChatURL pins the v3 Phase 1/4 gate: in bare mode
// (config_path empty) chat_url is required. v3 removed -base-url, so the
// endpoint must come from chat_url (or from config_path → miniagent.json).
func TestRun_BareModeRequiresChatURL(t *testing.T) {
	// api_key/model/workspace_root set; chat_url AND config_path both empty.
	p := writeMiniAgentConfig(t, `{
		"api_key":        "sk-test",
		"model":          "kimi",
		"workspace_root": "/tmp/miniagent-ws"
	}`)
	err := run(p)
	if err == nil {
		t.Fatal("run with empty chat_url AND config_path should return an error")
	}
	if !strings.Contains(err.Error(), "chat_url") || !strings.Contains(err.Error(), "bare mode") {
		t.Errorf("err = %q, want it to mention bare mode / chat_url", err.Error())
	}
}

// TestRun_ConfigPathSatisfiesBareGate pins the v3 Phase 4 path: a non-empty
// config_path satisfies the startup gate even when chat_url is empty. run()
// proceeds past the gate (it will still fail later — no real frontend, no
// miniagent binary — but the FAILURE MUST NOT be the bare-mode gate). This is
// the negative-space assertion for TestRun_BareModeRequiresChatURL.
func TestRun_ConfigPathSatisfiesBareGate(t *testing.T) {
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
	if strings.Contains(err.Error(), "bare mode") || strings.Contains(err.Error(), "chat_url") {
		t.Errorf("config_path set must satisfy the bare-mode gate; err = %q", err.Error())
	}
}
