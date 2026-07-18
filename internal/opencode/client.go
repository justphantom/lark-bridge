// Package opencode wraps the opencode CLI as the bridge's agent backend.
//
// This bridge shells out to the `opencode` binary in run/json mode per turn
// and consumes a stream of NDJSON events from stdout. A Run returns a channel
// of parsed Events terminated by a result or error event; the bridge's stream
// loop drives card updates from those events.
package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/justphantom/lark-bridge/internal/cliutil"
	"github.com/justphantom/lark-bridge/internal/log"
)

// readyTimeout bounds the `opencode --version` health check performed by
// IsReady. The opencode CLI loads provider/config state before printing the
// version and can take 6–11s on a warm cache (observed), so a 10s budget
// flakes and causes a startup crash-restart loop. 30s gives headroom while
// still failing fast on a genuinely missing or broken CLI.
const readyTimeout = 30 * time.Second

// defaultMaxConcurrent is the fallback concurrency cap when the config does
// not supply one.
const defaultMaxConcurrent = 4

// runEventChanBuf is the buffer size of the per-Run Event output channel. Large
// enough that a brief consumer stall does not block the subprocess pump, small
// enough to surface backpressure quickly.
const runEventChanBuf = 64

// Config is the subset of config.Opencode the CLI client actually needs.
// The shared config.Opencode struct still carries the legacy HTTP fields
// (base_url/username/password) which this client ignores; this struct
// captures the CLI-mode shape so client construction is self-documenting.
type Config struct {
	// CLIPath is the path to the opencode binary (default "opencode").
	CLIPath string
	// DefaultDirectory is the base dir for per-chat working directories.
	DefaultDirectory string
	// MaxConcurrent caps parallel opencode subprocesses (default 4).
	MaxConcurrent int
	// ListCacheTTL bounds how long ListModels/ListAgents results stay cached
	// (seconds). The opencode CLI takes 25-50s to list, so caching makes
	// repeated /model and /agent pickers instant. <=0 disables caching.
	ListCacheTTL int
}

// Client wraps the opencode CLI. It is safe for concurrent use: each Run
// spawns one subprocess, and a semaphore caps the number of parallel
// subprocesses at MaxConcurrent.
type Client struct {
	cliPath string
	logger  *log.Logger
	sem     chan struct{}

	// listTTL bounds the model/agent list cache. <=0 disables caching (every
	// call forks the CLI). listMu guards modelsCache/agentsCache.
	listTTL     time.Duration
	listMu      sync.Mutex
	modelsCache *listCache
	agentsCache *listCache
}

// New builds a Client from the CLI-mode config. The logger defaults to a
// no-op logger if nil.
func New(cfg Config, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Nop()
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	return &Client{
		cliPath: cfg.CLIPath,
		logger:  logger,
		sem:     make(chan struct{}, n),
		listTTL: time.Duration(cfg.ListCacheTTL) * time.Second,
	}
}

// RunOptions describes a single agent turn.
type RunOptions struct {
	// Prompt is sent to the CLI via stdin.
	Prompt string
	// Directory sets the subprocess working directory (cmd.Dir).
	Directory string
	// SessionID, when non-empty, is passed as --session to continue an
	// existing opencode session. Empty starts a fresh session; the
	// session id returned in the session.created event should be persisted
	// by the caller for subsequent turns.
	SessionID string
	// Model optionally overrides the configured model (--model).
	Model string
	// Agent optionally overrides the configured agent (--agent).
	Agent string
	// LineSink receives every raw stdout line verbatim before parsing.
	// Optional; nil disables teeing.
	LineSink io.Writer
}

// IsReady verifies the CLI is installed and invocable by running
// `<cliPath> --version`. Returns an error suitable for a startup health gate.
func (c *Client) IsReady(ctx context.Context) error {
	return cliutil.CheckVersion(ctx, c.cliPath, "opencode", readyTimeout, c.logger)
}

// Run starts one opencode CLI subprocess for opts and returns a channel of
// parsed Events. The channel is always closed by the client after the
// subprocess exits; a terminal Event (EventResult on success, EventError on
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

	cmd, err := c.buildCommand(opts)
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
		return nil, fmt.Errorf("start opencode: %w (is %s installed?)", err, c.cliPath)
	}

	// Prompt body is intentionally NOT logged here: the bridge layer logs a
	// redacted/truncated preview, and logging the raw prompt would leak
	// credentials/PII users pasted in.
	c.logger.Debug("opencode run started",
		log.FieldSessionID, opts.SessionID,
		"dir", opts.Directory,
		log.FieldPromptLength, len(opts.Prompt),
		log.FieldModel, opts.Model,
		"agent", opts.Agent)

	out := make(chan Event, runEventChanBuf)
	go c.pump(ctx, cmd, stdout, stderr, out, opts.LineSink)
	return out, nil
}

// buildCommand assembles the opencode CLI invocation for one turn.
// The prompt is passed as a positional argument (opencode run "<prompt>").
func (c *Client) buildCommand(opts RunOptions) (*exec.Cmd, error) {
	if c.cliPath == "" {
		return nil, errors.New("opencode: cli_path is empty")
	}
	args := []string{
		"run",
		"--format", "json",
		"--auto",
	}
	if opts.SessionID != "" {
		args = append(args, "--session", opts.SessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.Agent != "" {
		args = append(args, "--agent", opts.Agent)
	}
	// Prompt as positional arg — opencode run does not read stdin.
	args = append(args, opts.Prompt)

	// #nosec G204 -- c.cliPath comes from the trusted config file; args are
	// constructed internally (session/model/agent are validated upstream by
	// the slash commands before reaching here).
	cmd := exec.Command(c.cliPath, args...)
	if opts.Directory != "" {
		cmd.Dir = opts.Directory
	}
	// Put the CLI in its own process group so cancellation can SIGKILL the
	// whole tree (the CLI spawns tool subprocesses: bash, git, npm…). Without
	// this, Kill only reaches the CLI PID and its grandchildren are orphaned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}
