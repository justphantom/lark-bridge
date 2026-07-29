//go:build linux || darwin

// Package omp wraps the Oh My Pi (omp) CLI as the bridge's agent backend.
//
// This bridge shells out to the `omp` binary in -p/--mode json mode per turn
// and consumes a stream of NDJSON events from stdout. A Run returns a channel
// of parsed Events terminated by an agent_end or error event; the bridge's
// stream loop drives card updates from those events.
package omp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/justphantom/lark-bridge/internal/clibase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
)

// readyTimeout bounds the `omp --version` health check performed by IsReady.
// §A.5 observed omp/17.1.8 returns `omp --version` quickly, so a short budget
// fails fast on a missing/broken CLI without flirting with systemd's
// TimeoutStartSec.
const readyTimeout = 10 * time.Second

// defaultMaxConcurrent is the fallback concurrency cap when Options does not
// supply one.
const defaultMaxConcurrent = 4

// runEventChanBuf is the buffer size of the per-Run Event output channel.
// Large enough that a brief consumer stall does not block the subprocess
// pump, small enough to surface backpressure quickly.
const runEventChanBuf = 64

// Options configures a Client at construction time.
type Options struct {
	// CLIPath is the omp binary to invoke. Empty defaults to "omp"
	// (PATH lookup).
	CLIPath string
	// AppendSystemPrompt is passed verbatim as --append-system-prompt.
	AppendSystemPrompt string
	// MaxConcurrent caps parallel subprocesses. <=0 defaults to 4.
	MaxConcurrent int
	// Logger receives debug/warn lines. nil defaults to a no-op logger.
	Logger *log.Logger
}

// Client wraps the omp CLI. It is safe for concurrent use: each Run spawns
// one subprocess, and a semaphore caps the number of parallel subprocesses
// at MaxConcurrent.
type Client struct {
	cliPath            string
	appendSystemPrompt string
	logger             *log.Logger
	sem                chan struct{}
}

// New builds a Client from opts, applying the documented defaults for any
// zero-valued fields (see Options).
func New(opts Options) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = log.Nop()
	}
	cliPath := opts.CLIPath
	if cliPath == "" {
		cliPath = "omp"
	}
	n := opts.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	return &Client{
		cliPath:            cliPath,
		appendSystemPrompt: opts.AppendSystemPrompt,
		logger:             logger,
		sem:                make(chan struct{}, n),
	}
}

// RunOptions describes a single agent turn.
type RunOptions struct {
	// Prompt is passed as the positional argument (omp -p ... "<prompt>").
	// OMP does NOT read stdin (unlike claude), so the prompt must be a
	// positional arg appended last.
	Prompt string
	// Directory is passed as --cwd (OMP's working-dir flag, not cmd.Dir).
	Directory string
	// SessionID, when non-empty, is passed as --resume to continue an
	// existing omp session. Empty starts a fresh persisted session; the
	// session id from the `session` header is back-filled by the bridge
	// for the next turn. --no-session is intentionally NOT passed: it
	// suppresses persistence and breaks --resume (§5.2/§10.2).
	SessionID string
	// Model optionally overrides the configured model (--model).
	Model string
	// ApprovalMode is passed as --approval-mode (always-ask|write|yolo).
	// Empty falls back to the Client-level default in the bridge layer.
	ApprovalMode string
	// ThinkingLevel is passed as --thinking (off|minimal|low|medium|high|
	// xhigh|max|auto). Empty falls back to the bridge-level default.
	ThinkingLevel string
	// Tools is the --tools whitelist (verbatim CLI list syntax). Only
	// applied when NoTools is false.
	Tools string
	// NoTools passes --no-tools, disabling all tool use for the turn.
	NoTools bool
	// MaxTime is passed as --max-time (e.g. "5m"). Empty omits the flag:
	// OMP's --max-time aborts the turn with stopReason:"aborted" and drops
	// the current turn text (§A.6), so the bridge relies on ctx +
	// ApplyGroupCancel for the hard deadline instead. Only set when config
	// explicitly provides it.
	MaxTime string
	// LineSink receives every raw stdout line verbatim before parsing.
	// Optional; nil disables teeing.
	LineSink io.Writer
}

// IsReady verifies the CLI is installed and invocable by running
// `<cliPath> --version`. Returns an error suitable for a startup health gate.
func (c *Client) IsReady(ctx context.Context) error {
	return clibase.CheckVersion(ctx, c.cliPath, "omp", readyTimeout, c.logger)
}

// Run starts one omp CLI subprocess for opts and returns a channel of parsed
// Events. The channel is always closed by the client after the subprocess
// exits; a terminal Event (EventAgentEnd on success, EventError on
// failure/cancellation) is emitted immediately before close when the CLI
// itself did not emit one.
//
// The caller MUST drain the channel until it is closed. Run blocks acquiring
// a concurrency slot until ctx is cancelled (returning ctx.Err()) or a slot
// frees up.
func (c *Client) Run(ctx context.Context, opts RunOptions) (<-chan Event, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	cmd, err := c.buildCommand(ctx, opts)
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
		return nil, fmt.Errorf("start omp: %w (is %s installed?)", err, c.cliPath)
	}

	// Prompt body is intentionally NOT logged here: the bridge layer logs a
	// redacted/truncated preview, and logging the raw prompt would leak
	// credentials/PII users pasted in.
	c.logger.Debug("omp run started",
		log.FieldSessionID, opts.SessionID,
		"dir", opts.Directory,
		log.FieldPromptLength, len(opts.Prompt),
		log.FieldModel, opts.Model,
		"approval_mode", opts.ApprovalMode,
		"thinking", opts.ThinkingLevel)

	out := make(chan Event, runEventChanBuf)
	go c.pump(ctx, cmd, stdout, stderr, out, opts.LineSink)
	return out, nil
}

// buildCommand assembles the omp CLI invocation for one turn per §5.2.
// The prompt is passed as the positional argument (omp -p ... "<prompt>").
func (c *Client) buildCommand(ctx context.Context, opts RunOptions) (*exec.Cmd, error) {
	if c.cliPath == "" {
		return nil, errors.New("omp: cli_path is empty")
	}
	args := []string{
		"-p",
		"--mode", "json",
		"--approval-mode", opts.ApprovalMode,
		"--thinking", opts.ThinkingLevel,
	}
	if c.appendSystemPrompt != "" {
		args = append(args, "--append-system-prompt", c.appendSystemPrompt)
	}
	if opts.SessionID != "" {
		args = append(args, "--resume", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.NoTools {
		args = append(args, "--no-tools")
	} else if opts.Tools != "" {
		args = append(args, "--tools", opts.Tools)
	}
	if opts.MaxTime != "" {
		args = append(args, "--max-time", opts.MaxTime)
	}
	if opts.Directory != "" {
		args = append(args, "--cwd", opts.Directory)
	}
	// Prompt as positional arg — omp -p reads it from argv, not stdin.
	args = append(args, opts.Prompt)

	// #nosec G204 -- c.cliPath comes from the trusted config file; args are
	// constructed internally (session/model/approval/thinking are validated
	// upstream by the slash commands before reaching here).
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	// OMP takes its working dir via --cwd, but also set cmd.Dir when present
	// so any tool grandchild that ignores --cwd still lands in the right
	// place (defence in depth; matches the opencode/claude clients).
	if opts.Directory != "" {
		cmd.Dir = opts.Directory
	}
	// Sanitised env: strip the bridge's own secrets (FEISHU_APP_SECRET, IPC_
	// SECRET, ENCRYPT_KEY, …) so a user-run Bash tool inside the CLI cannot
	// read them. The CLI's own *_API_KEY survives.
	cmd.Env = cmdutil.SanitizeChildEnv()
	// ApplyGroupCancel puts the CLI in its own process group AND wires ctx
	// cancellation to SIGKILL the whole tree + bounds Wait via WaitDelay. A
	// tool grandchild that escapes the SIGKILL but keeps holding the stdout
	// pipe is force-closed within cmdutil.GroupKillTimeout, so cmd.Wait()
	// never blocks forever (a known hang mode for CLI backends).
	cmdutil.ApplyGroupCancel(cmd)
	return cmd, nil
}
