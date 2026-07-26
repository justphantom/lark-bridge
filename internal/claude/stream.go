//go:build linux || darwin

package claude

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf8"
)

// maxLineLen caps the per-line buffer for the stdout scanner. Claude
// stream-json lines are usually small but tool_result payloads (file
// reads, command output) can run to several MiB. The scanner buffer
// grows lazily to fit the largest line, so this is a per-run ceiling,
// not a pre-allocation; 16 MiB covers realistic tool output without
// letting a pathological stream exhaust memory.
const maxLineLen = 1 << 24

// maxStderrBytes bounds the stderr capture so a pathological CLI run
// cannot exhaust memory. The head of stderr is where the actionable
// diagnostic lives; 64 KiB is ample for that.
const maxStderrBytes = 64 << 10

// scannerInitBuf is the initial buffer for the stdout scanner. The scanner
// grows this lazily up to maxLineLen, so it is a starting allocation, not a cap.
const scannerInitBuf = 64 << 10

// maxLogLineBytes caps how much of an unparseable line is written to the log
// on a parse failure. A pathological line can be up to maxLineLen (16 MiB);
// logging it whole would bloat structured-log output, so only the head is kept.
const maxLogLineBytes = 1 << 10 // 1 KiB

// pump reads stdout lines from a started claude subprocess, parses each
// into Events, and forwards them to out. It owns the subprocess
// lifecycle from Start() to Wait(): on context cancellation it kills
// the process (which unblocks the scanner), then waits for it. Exactly
// one goroutine per Run. Releases the concurrency slot on exit and
// closes out.
func (c *Client) pump(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, out chan<- Event, sink io.Writer) {
	defer func() { <-c.sem }()
	defer close(out)

	// Best-effort stderr capture for diagnostics on abnormal exit.
	// Bounded by maxStderrBytes so a misbehaving CLI cannot OOM us.
	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&stderrBuf, io.LimitReader(stderr, maxStderrBytes))
		close(stderrDone)
	}()

	// ctx cancellation → SIGKILL the subprocess GROUP so the stdout reader
	// unblocks. The CLI runs in its own process group (Setpgid in buildCommand),
	// so a negative PID reaches the CLI plus any tool subprocesses it spawned
	// (bash, git, npm…). The CLI does not install its own signal handlers in
	// -p stream mode, so SIGKILL is sufficient.
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
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, scannerInitBuf), maxLineLen)

ScanLoop:
	for scanner.Scan() {
		line := scanner.Text()

		// Tee the raw line verbatim before parsing so the archive holds the
		// complete CLI return stream, including lines parseEvent rejects.
		if sink != nil {
			_, _ = io.WriteString(sink, line+"\n")
		}

		events, err := parseEvent(line)
		if err != nil {
			c.logger.Warn("parse claude event",
				"error", err,
				"line", truncate(line, maxLogLineBytes))
			continue
		}

		for _, ev := range events {
			if ev.Type == EventResult || ev.Type == EventError {
				sawTerminal = true
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				// Pipeline cancelled mid-event: stop forwarding and let
				// the shutdown path below synthesise a terminal event.
				break ScanLoop
			}
		}
	}

	<-stderrDone          // ensure stderr is fully captured before Wait
	waitErr := cmd.Wait() // reaps the (possibly killed) subprocess
	scanErr := scanner.Err()

	if !sawTerminal {
		c.emitTerminal(ctx, waitErr, scanErr, &stderrBuf, out)
	}
	if scanErr != nil && ctx.Err() == nil {
		c.logger.Warn("read claude stdout", "error", scanErr)
	}
}

// emitTerminal synthesises an EventError when the CLI exited without
// emitting a result/error event (e.g. crashed, killed on cancellation).
// scanErr is the stdout reader error, if any; a too-long line (huge
// tool_result) is surfaced here as the real cause rather than the
// generic "no result event" message. The send is guarded by ctx so a
// cancelled consumer cannot deadlock the pump; if the consumer is gone
// the error is logged instead of dropped silently.
func (c *Client) emitTerminal(ctx context.Context, waitErr, scanErr error, stderrBuf *bytes.Buffer, out chan<- Event) {
	msg := "claude exited without a result event"
	switch {
	case ctx.Err() != nil:
		msg = "claude run cancelled: " + ctx.Err().Error()
	case scanErr != nil:
		msg = "read claude stdout: " + scanErr.Error()
	case waitErr != nil:
		msg = waitErr.Error()
	}
	if stderrBuf.Len() > 0 {
		msg += "; stderr: " + strings.TrimSpace(stderrBuf.String())
	}
	ev := Event{Type: EventError, Text: msg}
	select {
	case out <- ev:
	case <-ctx.Done():
		c.logger.Warn("dropped terminal error event (consumer cancelled)", "error", errors.New(msg))
	}
}

// truncate shortens s to at most n bytes. If s is longer, the suffix
// "..." is appended so the total length is n+3. n must be > 0.
//
// The cut lands on a UTF-8 rune boundary so the result is always
// valid UTF-8 (a byte-boundary cut could split a multi-byte sequence
// in the middle of a Chinese character or emoji).
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// cut==0 means n is smaller than the first rune's byte length (e.g.
	// truncate("你好", 1)); s[:n] would split a multi-byte sequence. Return
	// just the ellipsis so the result stays valid UTF-8.
	if cut == 0 {
		return "..."
	}
	return s[:cut] + "..."
}
