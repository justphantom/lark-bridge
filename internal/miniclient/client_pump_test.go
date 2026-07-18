package miniclient

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReplaceArg_Existing replaces --flag <old> with <new> in place and
// leaves the surrounding args intact. Per-chat permission override relies
// on this mutation, not a duplicate append.
func TestReplaceArg_Existing(t *testing.T) {
	args := []string{"--model", "m", "--permission", "default", "--workdir", "/x"}
	got := replaceArg(args, "--permission", "free")
	want := []string{"--model", "m", "--permission", "free", "--workdir", "/x"}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("idx %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

// TestReplaceArg_Missing appends --flag newval when --flag is absent (the
// global default was "" so buildArgs never added it; per-chat override must
// introduce it).
func TestReplaceArg_Missing(t *testing.T) {
	args := []string{"--model", "m"}
	got := replaceArg(args, "--permission", "plan")
	want := []string{"--model", "m", "--permission", "plan"}
	if len(got) != len(want) {
		t.Fatalf("len got=%d want=%d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("idx %d: got=%q want=%q", i, got[i], want[i])
		}
	}
}

// TestReplaceArg_FlagAtTailNoValue does not panic when --flag is the last
// token (no value to replace). The walk stops at len-1 so it appends a
// fresh pair rather than mutating out of range.
func TestReplaceArg_FlagAtTailNoValue(t *testing.T) {
	args := []string{"--model", "m", "--permission"}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panicked: %v", r)
		}
	}()
	got := replaceArg(args, "--permission", "free")
	// Existing --permission has no value following it, so the loop misses it
	// and a new pair is appended.
	if !contains(got, "--permission") || !contains(got, "free") {
		t.Errorf("expected --permission free appended, got %v", got)
	}
}

// writeHelperScript writes a tiny bash script that emits the given stdout
// lines (one echo per line) then exits with code. Used to drive pump without
// the real miniagent binary.
func writeHelperScript(t *testing.T, lines []string, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "fakeagent.sh")
	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	for _, l := range lines {
		// printf so we control exact bytes (no trailing surprises).
		sb.WriteString("printf '%s\\n' " + quoteForSh(l) + "\n")
	}
	sb.WriteString("exit " + itoa(exitCode) + "\n")
	if err := os.WriteFile(p, []byte(sb.String()), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return p
}

func quoteForSh(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// TestRun_HappyPath verifies a subprocess emitting tool_use → result fires
// the matching Events in order, then closes the channel.
func TestRun_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := writeHelperScript(t, []string{
		`{"type":"tool_use","name":"read_file","input":"{}"}`,
		`{"type":"tool_result","name":"read_file","output":"hi"}`,
		`{"type":"result","text":"done","model":"m","steps":1}`,
	}, 0)
	c := New(Config{CLIPath: script}, nil)
	ch, err := c.Run(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var kinds []string
	for ev := range ch {
		kinds = append(kinds, ev.Kind)
	}
	want := []string{KindToolUse, KindToolResult, KindResult}
	if len(kinds) != len(want) {
		t.Fatalf("events=%v want=%v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("idx %d: got=%q want=%q", i, kinds[i], want[i])
		}
	}
}

// TestRun_CrashNoTerminal verifies a subprocess that exits non-zero without
// writing a terminal event triggers the synthesized KindError so the
// consumer's drain loop still terminates.
func TestRun_CrashNoTerminal(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	script := writeHelperScript(t, []string{
		`{"type":"tool_use","name":"x","input":"{}"}`,
	}, 3) // exit 3 before any terminal event
	c := New(Config{CLIPath: script}, nil)
	ch, err := c.Run(context.Background(), RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var last Event
	var count int
	for ev := range ch {
		last = ev
		count++
	}
	if count == 0 {
		t.Fatal("no events emitted; expected synthesized terminal error")
	}
	if !last.IsTerminal || last.Kind != KindError {
		t.Errorf("last event = %+v, want synthesized KindError terminal", last)
	}
	if !strings.Contains(last.Message, "miniagent exited") {
		t.Errorf("error message = %q, want it to mention exit", last.Message)
	}
}

// TestRun_AbortClosesChannel verifies ctx cancellation unblocks the event
// channel consumer within a bounded time (the group-kill + cmd.Wait path).
// Regression guard for the "abort hangs forever" class of bug.
func TestRun_AbortClosesChannel(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	// A script that sleeps indefinitely and emits nothing.
	dir := t.TempDir()
	p := filepath.Join(dir, "sleep.sh")
	if err := os.WriteFile(p, []byte("#!/bin/bash\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	c := New(Config{CLIPath: p}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Run(ctx, RunOptions{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		// Either the synthesized error event or close — both are fine; the
		// contract is "channel closes in bounded time after cancel".
		_ = ok
	case <-time.After(5 * time.Second):
		t.Fatal("channel did not close within 5s of cancel")
	}
}

// TestRun_EmptyCLIPath verifies the guard clause fires before touching the
// semaphore (so a misconfigured client does not leak a slot).
func TestRun_EmptyCLIPath(t *testing.T) {
	c := New(Config{}, nil)
	_, err := c.Run(context.Background(), RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "cli_path is empty") {
		t.Fatalf("err = %v, want cli_path empty error", err)
	}
}
