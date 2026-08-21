package ipcserver

import (
	"testing"
	"time"
)

// waitFor polls cond until it holds or the 1s deadline passes. Local copy of
// the frontend tests' helper (ipcserver tests moved out of the feishufront
// package in the 2026-08-19 sub-package split).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never became true within 1s")
}
