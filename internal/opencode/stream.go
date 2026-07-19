package opencode

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// maxLineLen caps the per-line buffer for the stdout scanner. opencode NDJSON
// lines are usually small but tool output payloads can run to several MiB.
// The scanner buffer grows lazily to fit the largest line, so this is a
// per-run ceiling, not a pre-allocation; 16 MiB covers realistic tool output
// without letting a pathological stream exhaust memory.
const maxLineLen = 1 << 24

// maxStderrBytes bounds the stderr capture so a pathological CLI run cannot
// exhaust memory. The head of stderr is where the actionable diagnostic
// lives; 64 KiB is ample for that.
const maxStderrBytes = 64 << 10

// scannerInitBuf is the initial buffer for the stdout scanner. The scanner
// grows this lazily up to maxLineLen, so it is a starting allocation, not a cap.
const scannerInitBuf = 64 << 10

// maxLogLineBytes caps how much of an unparseable line is written to the log
// on a parse failure. A pathological line can be up to maxLineLen (16 MiB);
// logging it whole would bloat structured-log output, so only the head is kept.
const maxLogLineBytes = 1 << 10 // 1 KiB

// pump reads stdout lines from a started opencode subprocess, parses each into
// Events, and forwards them to out. It owns the subprocess lifecycle from
// Start() to Wait(): on context cancellation it kills the process (which
// unblocks the scanner), then waits for it. Exactly one goroutine per Run.
// Releases the concurrency slot on exit and closes out.
func (c *Client) pump(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, out chan<- Event, sink io.Writer) {
	defer func() { <-c.sem }()
	defer close(out)

	// Best-effort stderr capture for diagnostics on abnormal exit. Bounded by
	// maxStderrBytes so a misbehaving CLI cannot OOM us.
	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrBuf, io.LimitReader(stderr, maxStderrBytes))
		close(stderrDone)
	}()

	// ctx cancellation → SIGKILL the subprocess GROUP so the stdout reader
	// unblocks. The CLI runs in its own process group (Setpgid in buildCommand),
	// so a negative PID reaches the CLI plus any tool subprocesses it spawned
	// (bash, git, npm…).
	killDone := make(chan struct{})
	defer close(killDone)
	go func() {
		select {
		case <-ctx.Done():
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-killDone:
		}
	}()

	sawTerminal := false
	lineCount := 0
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, scannerInitBuf), maxLineLen)

ScanLoop:
	for scanner.Scan() {
		line := scanner.Text()
		lineCount++

		// Tee the raw line verbatim before parsing so the archive holds the
		// complete CLI return stream, including lines parseEvent rejects.
		if sink != nil {
			_, _ = io.WriteString(sink, line+"\n") //nolint:gosec // G705: sink is a streamarchive file writer, not an HTTP response
		}

		events, err := parseEvent(line)
		if err != nil {
			c.logger.Warn("parse opencode event",
				log.FieldError, err,
				"line", strutil.Truncate(line, maxLogLineBytes))
			continue
		}

		for _, ev := range events {
			if ev.kind == EventResult || ev.kind == EventError {
				sawTerminal = true
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				// Pipeline cancelled mid-event: stop forwarding and let the
				// shutdown path below synthesise a terminal event.
				break ScanLoop
			}
		}
	}

	<-stderrDone          // ensure stderr is fully captured before Wait
	waitErr := cmd.Wait() // reaps the (possibly killed) subprocess
	scanErr := scanner.Err()

	if !sawTerminal {
		c.logger.Warn("opencode exited without terminal event",
			"stdout_lines", lineCount,
			"stderr_len", stderrBuf.Len())
		c.emitTerminal(ctx, waitErr, scanErr, &stderrBuf, out)
	}
	if scanErr != nil && ctx.Err() == nil {
		c.logger.Warn("read opencode stdout", log.FieldError, scanErr)
	}
}

// emitTerminal synthesises an EventError when the CLI exited without emitting
// a result/error event (e.g. crashed, killed on cancellation). scanErr is the
// stdout reader error, if any; a too-long line (huge tool output) is surfaced
// here as the real cause rather than the generic "no result event" message.
// The send is guarded by ctx so a cancelled consumer cannot deadlock the pump;
// if the consumer is gone the error is logged instead of dropped silently.
func (c *Client) emitTerminal(ctx context.Context, waitErr, scanErr error, stderrBuf *bytes.Buffer, out chan<- Event) {
	msg := "opencode exited without a result event"
	switch {
	case ctx.Err() != nil:
		msg = "opencode run cancelled: " + ctx.Err().Error()
	case scanErr != nil:
		msg = "read opencode stdout: " + scanErr.Error()
	case waitErr != nil:
		msg = waitErr.Error()
	}
	if stderrBuf.Len() > 0 {
		msg += "; stderr: " + strings.TrimSpace(stderrBuf.String())
	}
	ev := Event{kind: EventError, text: msg}
	select {
	case out <- ev:
	case <-ctx.Done():
		c.logger.Warn("dropped terminal error event (consumer cancelled)", log.FieldError, errors.New(msg))
	}
}
