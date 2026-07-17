// Package goose wraps the goose CLI as the bridge's agent backend.
//
// goose runs one subprocess per turn: `goose run -i - --output-format
// stream-json -q [--resume] --name <anchor> [--model M] [--max-turns N]`,
// reading the prompt from stdin and emitting one NDJSON event per stdout line.
// Unlike peri (stateless), goose keeps session state in a global SQLite DB and
// is resumed by name: the first run with `--name <anchor>` creates a session,
// subsequent runs add `--resume --name <anchor>` to continue it. A missing
// name under --resume fails with "No session found" — the bridge retries once
// without --resume (stale-session recovery, mirroring claude-back).
//
// goose ALWAYS exits 0 (verified: even an unknown-provider error returns 0),
// so success is judged by whether a complete event reached stdout, never by
// the exit code.
package goose

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

// readyTimeout bounds the `goose --version` health check.
const readyTimeout = 30 * time.Second

// defaultMaxConcurrent is the fallback concurrency cap.
const defaultMaxConcurrent = 4

// runEventChanBuf is the per-Run Event output channel buffer.
const runEventChanBuf = 64

// Config is the subset of config.Goose the CLI client needs.
type Config struct {
	// CLIPath is the path to the goose binary (default "goose").
	CLIPath string
	// DefaultDirectory is reserved for parity; goose takes its working dir
	// from the /cd pin (cmd.Dir), not from this field.
	DefaultDirectory string
	// MaxConcurrent caps parallel goose subprocesses (default 4).
	MaxConcurrent int
	// MaxTurns caps the agentic turns per run (passed as --max-turns). <=0 → 1.
	MaxTurns int
}

// Client wraps the goose CLI. Safe for concurrent use: each Run spawns one
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
	// Prompt is sent to the CLI via stdin (goose run -i - reads it from stdin).
	Prompt string
	// Directory sets cmd.Dir.
	Directory string
	// Model optionally overrides the configured model (--model).
	Model string
	// SessionName is the goose session anchor passed as --name. The bridge
	// stores this in the router binding as the "session id". Empty disables
	// both --name and --resume (one-shot --no-session run).
	SessionName string
	// Resume adds --resume alongside --name. The first turn for a binding is
	// a create (Resume=false); subsequent turns resume (Resume=true). On a
	// stale name the bridge retries once with Resume=false.
	Resume bool
	// LineSink, when non-nil, receives every raw stream-json line verbatim
	// before parsing. Used by the bridge to archive each run's NDJSON.
	LineSink io.Writer
}

// IsReady verifies the CLI is installed by running `<cliPath> --version`.
func (c *Client) IsReady(ctx context.Context) error {
	return cliutil.CheckVersion(ctx, c.cliPath, "goose", readyTimeout, c.logger)
}

// Run starts one goose subprocess for opts and returns a channel of parsed
// Events. The channel is always closed after the subprocess exits; a terminal
// Event (EventComplete on the complete line, EventError on failure/cancel/no-
// complete-EOF) precedes close.
//
// The caller MUST drain the channel until close. Run blocks on the semaphore
// until ctx is cancelled (returning ctx.Err()) or a slot frees.
func (c *Client) Run(ctx context.Context, opts RunOptions) (<-chan Event, error) {
	if c.cliPath == "" {
		return nil, errors.New("goose: cli_path is empty")
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
		return nil, fmt.Errorf("start goose: %w (is %s installed?)", err, c.cliPath)
	}

	// Prompt via stdin, then close to signal EOF (goose run -i - reads stdin).
	if _, werr := io.WriteString(stdin, opts.Prompt); werr != nil {
		_ = cmd.Process.Kill()
		<-c.sem
		return nil, fmt.Errorf("write prompt to stdin: %w", werr)
	}
	_ = stdin.Close()

	c.logger.Debug("goose run started",
		"dir", opts.Directory,
		"resume", opts.Resume,
		log.FieldPromptLength, len(opts.Prompt),
		log.FieldModel, opts.Model)

	out := make(chan Event, runEventChanBuf)
	go c.pump(ctx, cmd, stdout, stderr, out, opts.LineSink)
	return out, nil
}

// buildCommand assembles the goose invocation. The prompt is piped via stdin
// (goose run -i - reads from stdin). Session handling:
//   - SessionName="" → --no-session (one-shot, no persistence)
//   - Resume=false + SessionName → --name <anchor> (creates a session)
//   - Resume=true  + SessionName → --resume --name <anchor> (continues)
//
// goose-bundled extensions are NOT added: the user's ~/.config/goose/config.yaml
// profile is trusted to enable the tools the deployment wants.
func (c *Client) buildCommand(opts RunOptions) (*exec.Cmd, io.WriteCloser, error) {
	maxTurns := c.maxTurns
	if maxTurns <= 0 {
		maxTurns = 1
	}
	args := []string{
		"run",
		"-i", "-",
		"--output-format", "stream-json",
		"-q",
		"--max-turns", fmt.Sprintf("%d", maxTurns),
	}
	if opts.SessionName == "" {
		args = append(args, "--no-session")
	} else {
		if opts.Resume {
			args = append(args, "--resume")
		}
		args = append(args, "--name", opts.SessionName)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	// #nosec G204 -- cliPath from trusted config; args constructed internally.
	cmd := exec.Command(c.cliPath, args...)
	if opts.Directory != "" {
		cmd.Dir = opts.Directory
	}
	// Own process group so cancellation SIGKILLs the whole tree (goose spawns
	// tool subprocesses: bash, git, node…).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdin pipe: %w", err)
	}
	return cmd, stdin, nil
}

// pump reads stdout line by line, parses each into Events, and feeds out. On
// subprocess exit it synthesizes a terminal Event (Complete already emitted if
// the run was healthy; otherwise Error) then closes out.
//
// Because goose always exits 0, the pump tracks whether a complete event
// arrived: a clean EOF without one is a failure (model produced nothing, CLI
// startup crashed, or a pre-first-line cancel), surfaced as EventError with the
// accumulated stderr as the hint.
func (c *Client) pump(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, out chan<- Event, sink io.Writer) {
	defer func() {
		<-c.sem
		close(out)
	}()

	// Tee stderr to debug log; goose emits diagnostics + non-JSON errors there
	// (e.g. "No session found", unknown-provider errors). stderrDone
	// synchronizes the reader against cmd.Wait below.
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
			c.logger.Debug("goose stderr", "line", line)
		}
	}()

	textAcc := strings.Builder{}
	gotComplete := false
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
		// capture is verbatim. Best-effort: a write error is logged and never
		// fails the run.
		if sink != nil {
			if _, werr := sink.Write(append(line, '\n')); werr != nil {
				c.logger.Debug("goose: stream archive write", "error", werr)
			}
		}
		evs, terminal, err := parseLine(line, &textAcc)
		if err != nil {
			c.logger.Debug("goose: unparseable line", "error", err, "line", truncate(string(line), 200))
			continue
		}
		for _, ev := range evs {
			if !emit(ev) {
				break emitLoop
			}
		}
		if terminal {
			gotComplete = true
			break
		}
	}

	if lineCount == 0 {
		c.logger.Warn("goose: produced no stdout lines (archive will be empty)",
			"got_complete", gotComplete,
			"scan_err", scanner.Err())
	}

	// Drain unread stdout so cmd.Wait does not block on the pipe reader.
	stdoutDrained := make(chan struct{})
	go func() {
		defer close(stdoutDrained)
		_, _ = io.Copy(io.Discard, stdout)
	}()
	<-stdoutDrained
	waitErr := cmd.Wait()
	<-stderrDone

	// Context cancelled → Error event wins over a prior complete.
	if ctx.Err() != nil {
		select {
		case out <- Event{kind: EventError, text: "goose run cancelled", isError: true}:
		default:
		}
		return
	}

	// No complete event → failure regardless of exit code (goose exits 0 even
	// on error). Surface stderr as the hint; fall back to waitErr or a generic
	// message.
	if !gotComplete {
		hint := strings.TrimSpace(stderrBuf.String())
		if hint == "" {
			if waitErr != nil {
				hint = waitErr.Error()
			} else {
				hint = "goose 运行未产生结果事件"
			}
		}
		c.logger.Warn("goose: no complete event",
			"error", waitErr,
			"stderr", truncate(hint, 500),
			"line_count", lineCount)
		select {
		case out <- Event{kind: EventError, text: hint, isError: true}:
		default:
		}
		return
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
