package cmdutil

import (
	"bytes"
	"context"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestApplyGroupCancel_KillsGrandchildren is the regression test for the
// bug where ctx cancel SIGKILL'd the main process but a forked tool
// subprocess (bash → git, make → docker, etc.) inherited the stdout
// write end of the pipe and hung the reader.
//
// Without group-kill + WaitDelay, the bash process is reaped but the
// backgrounded `sleep 30 &` survives as an orphan holding the pipe open,
// so cmd.Wait blocks for the full 30s of sleep. With ApplyGroupCancel,
// the whole process group is SIGKILLed and Wait returns within
// GroupKillTimeout (test overrides it to keep the run fast).
func TestApplyGroupCancel_KillsGrandchildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// `sleep 30 &` forces a fork (no bash last-command exec optimisation);
	// `wait` keeps the parent alive so it is the one SIGKILL'd by Cancel.
	cmd := exec.CommandContext(ctx, "bash", "-c", "sleep 30 & wait")
	ApplyGroupCancel(cmd)
	cmd.WaitDelay = 500 * time.Millisecond // speed up the test
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	pgid := cmd.Process.Pid

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	cancel()
	select {
	case <-waitDone:
		// good
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return within 5s of cancel; grandchild kept pipe open")
	}

	// The whole process group must be gone: signal 0 probes "exists".
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("process group still alive after Wait returned")
	}
}

// TestApplyGroupCancel_CancelBeforeStart verifies Cancel returns
// os.ErrProcessDone when cmd.Process is nil (Start not called yet).
// This guards the nil-check inside the closure.
func TestApplyGroupCancel_CancelBeforeStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	cmd := exec.CommandContext(context.Background(), "true")
	ApplyGroupCancel(cmd)
	if cmd.Cancel == nil {
		t.Fatal("ApplyGroupCancel did not set Cancel")
	}
	if err := cmd.Cancel(); err == nil {
		t.Error("Cancel before Start returned nil error, want os.ErrProcessDone")
	}
}

// TestApplyGroupCancel_HappyPath verifies a normal exit still works:
// group-kill is a ctx-cancel path, the happy path must be unaffected.
func TestApplyGroupCancel_HappyPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	// Cancel can only be set on a cmd created via CommandContext.
	cmd := exec.CommandContext(context.Background(), "echo", "hello")
	ApplyGroupCancel(cmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Output: %v", err)
	}
	if !bytes.Contains(out, []byte("hello")) {
		t.Errorf("output = %q, want to contain hello", out)
	}
}

// TestRunCombinedBounded_Truncates verifies output past MaxCombinedOutput
// is dropped (the head is kept; tail is the caller's job via tailOutput).
func TestRunCombinedBounded_Truncates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// Emit ~10 MiB; the head must be kept, the tail dropped. The exact
	// truncation point is MaxCombinedOutput; we check the size only.
	out, err := RunCombinedBounded(context.Background(), "", "bash", "-c", "yes hello | head -c $((10*1024*1024))")
	if err != nil {
		t.Fatalf("RunCombinedBounded: %v", err)
	}
	if len(out) != MaxCombinedOutput {
		t.Errorf("len(out) = %d, want %d", len(out), MaxCombinedOutput)
	}
	// The head is preserved verbatim.
	if !strings.HasPrefix(string(out), "hello") {
		t.Errorf("head dropped: %q...", string(out)[:50])
	}
}

// TestRunCombinedBounded_KillsOnCancel verifies ctx cancel propagates as
// a group SIGKILL and Wait returns within GroupKillTimeout.
func TestRunCombinedBounded_KillsOnCancel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are POSIX-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = RunCombinedBounded(ctx, "", "bash", "-c", "sleep 30 & wait")
	}()
	// Give the subprocess a moment to Start so cmd.Process is non-nil.
	startTime := time.Now()
	cancel()
	select {
	case <-done:
		if d := time.Since(startTime); d > 5*time.Second {
			t.Errorf("Wait took %v; group-kill should make it < 5s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunCombinedBounded did not return within 10s of cancel")
	}
}

// TestLimitedWriter_Drops writes past max and reports full length so the
// pipe copier does not see a short write.
func TestLimitedWriter_Drops(t *testing.T) {
	w := newLimitedWriter(8)
	n, err := w.Write([]byte("0123456789ABCDEF")) // 16 bytes
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 16 {
		t.Errorf("reported n = %d, want 16 (full length)", n)
	}
	if got := string(w.bytes()); got != "01234567" {
		t.Errorf("buf = %q, want 01234567", got)
	}
	// Subsequent writes are fully dropped but still report full length.
	n, _ = w.Write([]byte("XYZ"))
	if n != 3 {
		t.Errorf("post-fill n = %d, want 3", n)
	}
	if got := string(w.bytes()); got != "01234567" {
		t.Errorf("buf changed after fill: %q", got)
	}
}
