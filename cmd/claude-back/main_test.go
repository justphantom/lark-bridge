package main

import "testing"

// TestRun_BadConfigReturnsError pins the fail-fast contract: a missing
// config path makes run() return an error (which main promotes to
// os.Exit(1)) rather than crashing or silently proceeding. The error path
// is the only part of main worth pinning without spawning a real claude
// subprocess; happy paths are covered by internal/claudebridge tests.
func TestRun_BadConfigReturnsError(t *testing.T) {
	if err := run("/nonexistent/lark-claude-back-config.json"); err == nil {
		t.Fatal("run with missing config should return an error")
	}
}
