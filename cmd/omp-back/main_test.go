package main

import "testing"

// TestCLIRunner_BadConfigReturnsError pins the fail-fast contract: a missing
// config path makes CLIRunner.Run return an error (which main promotes to
// os.Exit(1)) rather than crashing or silently proceeding. The error path
// is the only part of main worth pinning without spawning a real omp
// subprocess; happy paths are covered by internal/ompbridge tests.
func TestCLIRunner_BadConfigReturnsError(t *testing.T) {
	runner := buildOmpRunner()
	if err := runner.Run("/nonexistent/lark-omp-back-config.json", "dev"); err == nil {
		t.Fatal("CLIRunner.Run with missing config should return an error")
	}
}
