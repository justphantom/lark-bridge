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

	"github.com/hu/lark-bridge/internal/cliutil"
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
	// Effort optionally overrides the reasoning effort (--effort). Peri
	// accepts low/medium/high/max. Empty falls back to the peri default.
	Effort string
	// PermissionMode optionally overrides the permission mode
	// (--permission-mode). Peri accepts bypass/default/accept-edit/auto-mode.
	// Empty falls back to bypass (backends run unattended).
	PermissionMode string
	// SettingsFile optionally loads an extra settings file or JSON string
	// (--settings). Empty disables the flag.
	SettingsFile string
	// LineSink, when non-nil, receives every raw stream-json line verbatim
	// before parsing. Used by the bridge to archive each run's NDJSON under
	// {state_dir}/streams with rotation (StreamHistory cap). nil disables tee.
	LineSink io.Writer
}

// IsReady verifies the CLI is installed by running `<cliPath> --version`.
func (c *Client) IsReady(ctx context.Context) error {
	return cliutil.CheckVersion(ctx, c.cliPath, "peri", readyTimeout, c.logger)
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
	go c.pump(ctx, cmd, stdout, stderr, out, opts.LineSink)
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
	}
	// Permission mode: default to bypass (backends run unattended; HITL is
	// handled by the bridge's own Question/Permission controls, not peri's
	// TUI approval). A non-empty override from /perm replaces it. "default"
	// would deadlock the non-interactive -p subprocess; the bridge's /perm
	// picker excludes it, but a direct pin is still honoured here so a
	// misconfigured picker surfaces the hang rather than silently rewriting.
	permMode := opts.PermissionMode
	if permMode == "" {
		permMode = "bypass"
	}
	args = append(args, "--permission-mode", permMode)
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}
	if opts.SettingsFile != "" {
		args = append(args, "--settings", opts.SettingsFile)
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
func (c *Client) pump(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, out chan<- Event, sink io.Writer) {
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
	var lineCount int
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
		lineCount++
		// Tee the raw line to the archive sink before parsing so the on-disk
		// capture is verbatim (matching claude/opencode bridges). Best-effort:
		// a write error is logged at debug and never fails the run.
		if sink != nil {
			if _, werr := sink.Write(append(line, '\n')); werr != nil {
				c.logger.Debug("peri: stream archive write", "error", werr)
			}
		}
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

	// Surface the 0-byte-archive root cause: a run that opened an archive
	// file but emitted no stdout line (lineCount==0) left a 0-byte file on
	// disk. Distinguishes "peri produced nothing" (model empty reply, CLI
	// startup crash with stderr only, or pre-first-line cancel) from a
	// healthy run. Logged at Warn so it stands out without needing debug.
	if lineCount == 0 {
		c.logger.Warn("peri: produced no stdout lines (archive will be empty)",
			"got_terminal", gotTerminal,
			"scan_err", scanner.Err())
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
		// Log the exit error + stderr separately from the synthesized event so
		// a 0-byte-archive crash is diagnosable: lineCount (Warn above) shows
		// no stdout was produced, and this Warn shows why peri exited.
		c.logger.Warn("peri: subprocess exited non-zero",
			"error", waitErr,
			"stderr", truncate(hint, 500),
			"line_count", lineCount)
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
