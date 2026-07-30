//go:build linux || darwin

package opencode

import (
	"context"
	"os/exec"
	"testing"

	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
)

// TestPump_OversizedLineDoesNotAbortTurn is the F1 guard: a stdout line
// larger than maxLineLen is truncated and counted, and the turn continues —
// the following terminal event still arrives instead of the run aborting
// with bufio.ErrTooLong the way the old scanner-based pump did.
func TestPump_OversizedLineDoesNotAbortTurn(t *testing.T) {
	eventmetrics.ResetAll()
	c := New(Config{CLIPath: "opencode"}, log.Nop())
	cmd := exec.Command("sh", "-c",
		`{ head -c 17000000 /dev/zero | tr '\0' 'x'; printf '\n'; printf '%s\n' '{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":10,"input":5,"output":5,"cache":{"read":0,"write":0}},"cost":0.01}}'; }`)
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
		if ev.kind == EventError {
			t.Fatalf("turn aborted on oversized line: %q", ev.text)
		}
		if ev.kind == EventResult {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatal("missing terminal result event after oversized line")
	}
	if got := eventmetrics.LineTruncated("opencode").Value(); got < 1 {
		t.Errorf("LineTruncated(opencode) = %d, want >= 1", got)
	}
}
