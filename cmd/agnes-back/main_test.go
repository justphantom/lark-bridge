package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRun_BadConfigReturnsError pins the fail-fast contract: a missing config
// path makes run() return an error (which main promotes to os.Exit(1)) rather
// than crashing or silently proceeding. The error path is the only part of main
// worth pinning without a live frontend + Agnes API; happy paths are covered by
// internal/agnesback tests. Mirrors cmd/deploy-monitor/main_test.go.
func TestRun_BadConfigReturnsError(t *testing.T) {
	if err := run("/nonexistent/lark-agnes-back-config.json"); err == nil {
		t.Fatal("run with missing config should return an error")
	}
}

// TestRun_MissingAPIKeyReturnsError pins the binary-specific required check: a
// config that validates structurally but lacks agnes.api_key surfaces a clear
// startup error instead of a confusing 401 on the first /image call. Writes a
// throwaway config file under the test temp dir.
func TestRun_MissingAPIKeyReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// A minimal valid-enough config: IPC three-piece set, no agnes.api_key.
	body := `{
		"backend_id": "agnes-1",
		"ipc_secret": "s",
		"frontend_url": "http://127.0.0.1:6060",
		"agnes": {}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	if err := run(path); err == nil {
		t.Fatal("run with missing agnes.api_key should return an error")
	}
}
