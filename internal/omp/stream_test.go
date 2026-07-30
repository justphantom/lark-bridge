//go:build linux || darwin

package omp

import (
	"context"
	"os/exec"
	"testing"

	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
)

// TestPump_OversizedLineDoesNotAbortTurn is the F1 guard: a stdout line
// larger than maxLineLen is truncated and counted, and the turn continues —
// the following agent_end still arrives instead of the run aborting with
// bufio.ErrTooLong the way the old scanner-based pump did.
func TestPump_OversizedLineDoesNotAbortTurn(t *testing.T) {
	eventmetrics.ResetAll()
	c := New(Options{CLIPath: "omp", Logger: log.Nop()})
	cmd := exec.Command("sh", "-c",
		`{ head -c 17000000 /dev/zero | tr '\0' 'x'; printf '\n'; printf '%s\n' '{"type":"agent_end","isTerminal":true}'; }`)
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

	var sawTerminal bool
	for ev := range out {
		if ev.Type == EventError {
			t.Fatalf("turn aborted on oversized line: %q", ev.Text)
		}
		if ev.Type == EventAgentEnd {
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Fatal("missing terminal agent_end event after oversized line")
	}
	if got := eventmetrics.LineTruncated("omp").Value(); got < 1 {
		t.Errorf("LineTruncated(omp) = %d, want >= 1", got)
	}
}
