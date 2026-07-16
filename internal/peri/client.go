// Package peri wraps the peri CLI as the bridge's agent backend.
//
// Phase 1 connectivity: peri runs in print mode (peri -p --output-format
// stream-json), one subprocess per turn, reading the prompt from stdin and
// emitting one NDJSON event per stdout line. Print mode has NO session
// persistence (history: vec![] in cli_print.rs), so each turn is stateless;
// multi-turn continuity requires ACP mode (out of Phase 1 scope).
package peri

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// readyTimeout bounds the `peri --version` health check.
const readyTimeout = 30 * time.Second

// defaultMaxConcurrent is the fallback concurrency cap.
const defaultMaxConcurrent = 4

// runEventChanBuf is the per-Run Event output channel buffer.
const runEventChanBuf = 64

// Config is the subset of config.Peri the CLI client needs.
type Config struct {
	// CLIPath is the path to the peri binary (default "peri").
	CLIPath string
	// DefaultDirectory is the base dir for per-chat working directories.
	DefaultDirectory string
	// MaxConcurrent caps parallel peri subprocesses (default 4).
	MaxConcurrent int
	// MaxTurns caps the agentic turns per run (passed as --max-turns). <=0 → 1.
	MaxTurns int
}

// Client wraps the peri CLI. Safe for concurrent use: each Run spawns one
// subprocess, capped by a semaphore at MaxConcurrent.
type Client struct {
	cliPath  string
	maxTurns int
	logger   *log.Logger
	sem      chan struct{}
}

// New builds a Client. logger defaults to no-op if nil.
func New(cfg Config, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Nop()
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	return &Client{
		cliPath:  cfg.CLIPath,
		maxTurns: cfg.MaxTurns,
		logger:   logger,
		sem:      make(chan struct{}, n),
	}
}

// RunOptions describes one agent turn.
type RunOptions struct {
	// Prompt is sent to the CLI via stdin (peri print mode does NOT accept a
	// positional prompt arg — verified empirically; it must come via stdin).
	Prompt string
	// Directory sets cmd.Dir.
	Directory string
	// Model optionally overrides the configured model (--model alias/name).
	Model string
}

// IsReady verifies the CLI is installed by running `<cliPath> --version`.
func (c *Client) IsReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	// #nosec G204 -- cliPath from trusted config.
	cmd := exec.CommandContext(ctx, c.cliPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("peri CLI not ready (%s --version): %w", c.cliPath, err)
	}
	c.logger.Info("peri CLI ready",
		"cli_path", c.cliPath,
		"version", strings.TrimSpace(string(out)))
	return nil
}

// Run starts one peri subprocess for opts and returns a channel of parsed
// Events. The channel is always closed after the subprocess exits; a terminal
// Event (EventResult on EOF, EventError on failure/cancel) precedes close.
//
// The caller MUST drain the channel until close. Run blocks on the semaphore
// until ctx is cancelled (returning ctx.Err()) or a slot frees.
func (c *Client) Run(ctx context.Context, opts RunOptions) (<-chan Event, error) {
	if c.cliPath == "" {
		return nil, errors.New("peri: cli_path is empty")
	}

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	cmd, stdin, err := c.buildCommand(opts)
	if err != nil {
		<-c.sem
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		<-c.sem
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		<-c.sem
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		<-c.sem
		return nil, fmt.Errorf("start peri: %w (is %s installed?)", err, c.cliPath)
	}

	// Prompt via stdin, then close to signal EOF.
	if _, werr := io.WriteString(stdin, opts.Prompt); werr != nil {
		_ = cmd.Process.Kill()
		<-c.sem
		return nil, fmt.Errorf("write prompt to stdin: %w", werr)
	}
	_ = stdin.Close()

	c.logger.Debug("peri run started",
		"dir", opts.Directory,
		log.FieldPromptLength, len(opts.Prompt),
		log.FieldModel, opts.Model)

	out := make(chan Event, runEventChanBuf)
	go c.pump(ctx, cmd, stdout, stderr, out)
	return out, nil
}

// buildCommand assembles the peri print-mode invocation. The prompt is NOT a
// positional arg (peri rejects it); it is piped via stdin.
func (c *Client) buildCommand(opts RunOptions) (*exec.Cmd, io.WriteCloser, error) {
	maxTurns := c.maxTurns
	if maxTurns <= 0 {
		maxTurns = 1
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--max-turns", fmt.Sprintf("%d", maxTurns),
		// bypass: backend runs unattended; HITL is handled by the bridge's
		// own Question/Permission controls, not peri's TUI approval.
		"--permission-mode", "bypass",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	// #nosec G204 -- cliPath from trusted config; args constructed internally.
	cmd := exec.Command(c.cliPath, args...)
	if opts.Directory != "" {
		cmd.Dir = opts.Directory
	}
	// Own process group so cancellation SIGKILLs the whole tree (peri spawns
	// tool subprocesses: bash, git, node…).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	return cmd, stdin, nil
}

// pump reads stdout line by line, parses each into Events, and feeds out. On
// subprocess exit it synthesises a terminal Event (Result on clean EOF with no
// prior terminal, Error on non-zero exit / cancel) then closes out.
func (c *Client) pump(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, out chan<- Event) {
	defer func() {
		<-c.sem
		close(out)
	}()

	// Tee stderr to debug log; peri emits diagnostics there on failure.
	// stderrDone synchronizes the stderr reader against the cmd.Wait + read
	// of stderrBuf below. Wait closes the subprocess's pipe write ends, the
	// reader's Scan unblocks with EOF, and stderrDone closes — so every read
	// of stderrBuf happens-after the last write.
	var (
		stderrBuf  strings.Builder
		stderrDone = make(chan struct{})
	)
	go func() {
		defer close(stderrDone)
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 64*1024), 1024*1024)
		for s.Scan() {
			line := s.Text()
			stderrBuf.WriteString(line)
			stderrBuf.WriteByte('\n')
			c.logger.Debug("peri stderr", "line", line)
		}
	}()

	textAcc := strings.Builder{}
	gotTerminal := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	emit := func(ev Event) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

emitLoop:
	for scanner.Scan() {
		line := scanner.Bytes()
		evs, terminal, err := parseLine(line, &textAcc)
		if err != nil {
			c.logger.Debug("peri: unparseable line", "error", err, "line", truncate(string(line), 200))
			continue
		}
		for _, ev := range evs {
			if !emit(ev) {
				break emitLoop
			}
		}
		if terminal {
			gotTerminal = true
			break
		}
	}

	// Drain any unread stdout so cmd.Wait does not block waiting on the pipe
	// reader. This covers both the terminal-event early break and the
	// cancel-during-emit break; on clean EOF Scan already reached the end and
	// the drain is a no-op. Done in a goroutine because cancel may leave a
	// half-written pipe whose far end is only closed once the SIGKILLed group
	// exits (which happens in Wait, after this drain starts).
	stdoutDrained := make(chan struct{})
	go func() {
		defer close(stdoutDrained)
		_, _ = io.Copy(io.Discard, stdout)
	}()

	// Drain stdout fully before Wait: os/exec requires StdoutPipe's reader be
	// drained (or closed) before Wait returns, else Wait blocks. The drain
	// goroutine unblocks once the SIGKILLed subprocess closes its write end.
	<-stdoutDrained
	waitErr := cmd.Wait()
	<-stderrDone

	// Context cancelled → Error event wins over a synthesized Result.
	if ctx.Err() != nil {
		select {
		case out <- Event{kind: EventError, text: "peri run cancelled", isError: true}:
		default:
		}
		return
	}

	// Non-zero exit (not due to cancel) → surface stderr as an error.
	if waitErr != nil {
		hint := strings.TrimSpace(stderrBuf.String())
		if hint == "" {
			hint = waitErr.Error()
		}
		select {
		case out <- Event{kind: EventError, text: hint, isError: true}:
		default:
		}
		return
	}

	// Clean exit with no terminal event → synthesize Result from accumulated
	// text (stream-json has no result line; EOF is the only completion signal).
	if !gotTerminal {
		reply := textAcc.String()
		select {
		case out <- Event{kind: EventResult, result: reply}:
		default:
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
