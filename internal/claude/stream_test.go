package claude

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestEmitTerminal_SurfacesScanError locks in that a stdout read failure
// (e.g. a tool_result line exceeding maxLineLen) is surfaced as the
// terminal event's cause rather than the generic "no result event" msg.
func TestEmitTerminal_SurfacesScanError(t *testing.T) {
	c := New(configForTest(), nil)
	out := make(chan Event, 1)
	c.emitTerminal(context.Background(), nil, bufio.ErrTooLong, &bytes.Buffer{}, out)
	ev := <-out
	if ev.kind != EventError {
		t.Fatalf("kind = %q, want %q", ev.kind, EventError)
	}
	if !strings.Contains(ev.text, "token too long") {
		t.Fatalf("expected scan error surfaced in text, got %q", ev.text)
	}
}

// TestEmitTerminal_FallsBackToWaitError ensures a non-nil waitErr still
// drives the message when no scan error is present, and that stderr is
// appended.
func TestEmitTerminal_FallsBackToWaitError(t *testing.T) {
	c := New(configForTest(), nil)
	out := make(chan Event, 1)
	stderr := bytes.NewBufferString("panic: oom")
	c.emitTerminal(context.Background(), errors.New("exit status 1"), nil, stderr, out)
	ev := <-out
	if !strings.Contains(ev.text, "exit status 1") {
		t.Fatalf("expected waitErr in text, got %q", ev.text)
	}
	if !strings.Contains(ev.text, "panic: oom") {
		t.Fatalf("expected stderr appended to text, got %q", ev.text)
	}
}

// TestPump_TeesRawLinesToSink drives pump with a tiny subprocess that emits
// one parseable and one unparseable line, and asserts the sink captured both
// verbatim — the archive must hold the complete CLI return stream, including
// lines parseEvent rejects. The subprocess is the only honest way to exercise
// pump's scanner→parse→forward path end to end.
func TestPump_TeesRawLinesToSink(t *testing.T) {
	c := New(configForTest(), nil)
	cmd := exec.Command("sh", "-c",
		`printf '%s\n%s\n' '{"type":"system","subtype":"init","session_id":"s"}' 'NOT-JSON-GARBAGE'`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	var sink bytes.Buffer
	out := make(chan Event, 16)
	// Run acquires the concurrency slot before spawning pump; pump's deferred
	// <-c.sem balances it, so the test must acquire here too.
	c.sem <- struct{}{}
	c.pump(context.Background(), cmd, stdout, stderr, out, &sink)
	for range out {
	}

	got := sink.String()
	if !strings.Contains(got, `{"type":"system","subtype":"init","session_id":"s"}`) {
		t.Fatalf("sink missing the parseable line, got %q", got)
	}
	if !strings.Contains(got, "NOT-JSON-GARBAGE") {
		t.Fatalf("sink missing the unparseable line (tee must precede parse), got %q", got)
	}
	if !strings.HasSuffix(got, "NOT-JSON-GARBAGE\n") {
		t.Fatalf("each raw line must be followed by a newline, got %q", got)
	}
}
