//go:build linux || darwin

package claude

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"os/exec"
	"strings"
	"testing"
)

// TestEmitTerminal_SurfacesScanError locks in that a stdout read failure
// (e.g. a tool_result line exceeding maxLineLen) is surfaced as the
// terminal event's cause rather than the generic "no result event" msg.
func TestEmitTerminal_SurfacesScanError(t *testing.T) {
	c := New(optionsForTest())
	out := make(chan Event, 1)
	c.emitTerminal(context.Background(), nil, bufio.ErrTooLong, &bytes.Buffer{}, out)
	ev := <-out
	if ev.Type != EventError {
		t.Fatalf("Type = %q, want %q", ev.Type, EventError)
	}
	if !strings.Contains(ev.Text, "token too long") {
		t.Fatalf("expected scan error surfaced in text, got %q", ev.Text)
	}
}

// TestEmitTerminal_FallsBackToWaitError ensures a non-nil waitErr still
// drives the message when no scan error is present, and that stderr is
// appended.
func TestEmitTerminal_FallsBackToWaitError(t *testing.T) {
	c := New(optionsForTest())
	out := make(chan Event, 1)
	stderr := bytes.NewBufferString("panic: oom")
	c.emitTerminal(context.Background(), errors.New("exit status 1"), nil, stderr, out)
	ev := <-out
	if !strings.Contains(ev.Text, "exit status 1") {
		t.Fatalf("expected waitErr in text, got %q", ev.Text)
	}
	if !strings.Contains(ev.Text, "panic: oom") {
		t.Fatalf("expected stderr appended to text, got %q", ev.Text)
	}
}

// TestPump_TeesRawLinesToSink drives pump with a tiny subprocess that emits
// one parseable and one unparseable line, and asserts the sink captured both
// verbatim — the archive must hold the complete CLI return stream, including
// lines parseEvent rejects. The subprocess is the only honest way to exercise
// pump's scanner→parse→forward path end to end.
func TestPump_TeesRawLinesToSink(t *testing.T) {
	c := New(optionsForTest())
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

// TestPump_OversizedLineDoesNotAbortTurn is the F1 guard: a stdout line
// larger than maxLineLen is truncated and counted, and the turn continues —
// the following result event still arrives instead of the run aborting with
// bufio.ErrTooLong the way the old scanner-based pump did.
func TestPump_OversizedLineDoesNotAbortTurn(t *testing.T) {
	eventmetrics.ResetAll()
	c := New(optionsForTest())
	cmd := exec.Command("sh", "-c",
		`{ head -c 17000000 /dev/zero | tr '\0' 'x'; printf '\n'; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"survived","session_id":"s"}'; }`)
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

	out := make(chan Event, 16)
	c.sem <- struct{}{} // balances pump's deferred release (Run acquires it)
	c.pump(context.Background(), cmd, stdout, stderr, out, nil)

	var sawResult bool
	for ev := range out {
		if ev.Type == EventError {
			t.Fatalf("turn aborted on oversized line: %q", ev.Text)
		}
		if ev.Type == EventResult {
			sawResult = true
			if ev.Result != "survived" {
				t.Errorf("result = %q, want %q", ev.Result, "survived")
			}
		}
	}
	if !sawResult {
		t.Fatal("missing terminal result event after oversized line")
	}
	if got := eventmetrics.LineTruncated("claude").Value(); got < 1 {
		t.Errorf("LineTruncated(claude) = %d, want >= 1", got)
	}
}
