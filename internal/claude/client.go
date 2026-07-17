// Package claude wraps the Claude Code CLI as the bridge's agent backend.
//
// This bridge shells out to the `claude` binary in print/stream-json mode
// per turn and consumes a stream of events from stdout. A Run returns a
// channel of parsed Events terminated by a result or error event; the
// bridge's stream loop drives card updates from those events.
package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hu/lark-bridge/internal/cliutil"
	"github.com/hu/lark-bridge/internal/config"
	"github.com/hu/lark-bridge/internal/log"
)

// readyTimeout bounds the `claude --version` health check performed by
// IsReady. Kept short so a missing/misconfigured CLI fails fast instead
// of blocking startup beyond systemd's TimeoutStartSec.
const readyTimeout = 10 * time.Second

// defaultMaxConcurrent is the fallback concurrency cap when the config does
// not supply one.
const defaultMaxConcurrent = 4

// runEventChanBuf is the buffer size of the per-Run Event output channel. Large
// enough that a brief consumer stall does not block the subprocess pump, small
// enough to surface backpressure quickly.
const runEventChanBuf = 64

// Client wraps the Claude Code CLI. It is safe for concurrent use: each
// Run spawns one subprocess, and a semaphore caps the number of parallel
// subprocesses at MaxConcurrent.
type Client struct {
	cliPath            string
	permissionMode     string
	appendSystemPrompt string
	logger             *log.Logger
	sem                chan struct{}

	// settingsDir is scanned by ListSettings for the interactive /settings
	// picker. Resolved from config.Claude.SettingsDir at New time (empty →
	// ~/.claude). settingsTTL bounds the cache; <=0 disables caching.
	settingsDir   string
	settingsTTL   time.Duration
	settingsMu    sync.Mutex
	settingsCache *settingsListCache
}

// New builds a Client from the Claude config block. The logger defaults
// to a no-op logger if nil.
func New(cfg config.Claude, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Nop()
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	return &Client{
		cliPath:            cfg.CLIPath,
		permissionMode:     cfg.PermissionMode,
		appendSystemPrompt: cfg.AppendSystemPrompt,
		logger:             logger,
		sem:                make(chan struct{}, n),
		settingsDir:        resolveSettingsDir(cfg.SettingsDir),
		settingsTTL:        time.Duration(cfg.SettingsCacheTTL) * time.Second,
	}
}

// RunOptions describes a single agent turn.
type RunOptions struct {
	// Prompt is sent to the CLI via stdin.
	Prompt string
	// Directory sets the subprocess working directory (cmd.Dir).
	Directory string
	// SessionID, when non-empty, is passed as --resume to continue an
	// existing Claude session. Empty starts a fresh session; the
	// session_id returned in the system/init event should be persisted
	// by the caller for subsequent turns.
	SessionID string
	// Model optionally overrides the configured model (--model).
	Model string
	// PermissionMode optionally overrides the Client's configured
	// --permission-mode for this turn (per-chat pin from the binding).
	// Empty falls back to c.permissionMode. Validated upstream by the
	// /perm command; "default" never reaches here.
	PermissionMode string
	// EffortLevel optionally sets the Claude --effort level for this
	// turn (per-chat pin from the binding). Empty falls back to Claude's
	// default effort behavior. Validated upstream by the /effort command.
	EffortLevel string
	// SettingsFile optionally sets the Claude --settings file path for
	// this turn (per-chat pin from the binding). Empty means "not set".
	// The caller is responsible for any env-var expansion before passing
	// the path here; the client appends it verbatim to the CLI args.
	SettingsFile string
	// LineSink, when non-nil, receives every raw stream-json line verbatim
	// (line + "\n") as read from stdout, before parsing. Used by the bridge
	// to archive the complete CLI return stream. Writes are best-effort:
	// errors are ignored so an archive failure can never fail the run.
	LineSink io.Writer
}

// IsReady verifies the CLI is installed and invocable by running
// `<cliPath> --version`. Returns an error suitable for a startup health
// gate.
func (c *Client) IsReady(ctx context.Context) error {
	return cliutil.CheckVersion(ctx, c.cliPath, "claude", readyTimeout, c.logger,
		log.FieldPermissionMode, c.permissionMode)
}

// Run starts one Claude Code CLI subprocess for opts and returns a
// channel of parsed Events. The channel is always closed by the client
// after the subprocess exits; a terminal Event (EventResult on success,
// EventError on failure/cancellation) is emitted immediately before
// close when the CLI itself did not emit one.
//
// The caller MUST drain the channel until it is closed. Run blocks
// acquiring a concurrency slot until ctx is cancelled (returning
// ctx.Err()) or a slot frees up.
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
	cmd.Stdin = strings.NewReader(opts.Prompt)

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
		return nil, fmt.Errorf("start claude: %w (is %s installed?)", err, c.cliPath)
	}

	// Prompt body is intentionally NOT logged here: the bridge layer logs
	// a redacted/truncated preview, and this client has no redact flag —
	// logging the raw prompt would leak credentials/PII users pasted in.
	c.logger.Debug("claude run started",
		log.FieldSessionID, opts.SessionID,
		"dir", opts.Directory,
		log.FieldPromptLength, len(opts.Prompt),
		log.FieldModel, opts.Model,
		log.FieldPermissionMode, opts.PermissionMode,
		log.FieldEffortLevel, opts.EffortLevel,
		"settings_file", opts.SettingsFile)

	out := make(chan Event, runEventChanBuf)
	go c.pump(ctx, cmd, stdout, stderr, out, opts.LineSink)
	return out, nil
}

// buildCommand assembles the claude CLI invocation for one turn.
func (c *Client) buildCommand(opts RunOptions) (*exec.Cmd, error) {
	if c.cliPath == "" {
		return nil, errors.New("claude: cli_path is empty")
	}
	// Per-turn permission mode override (per-chat pin); fall back to
	// the Client's configured default when the caller left it empty.
	permMode := opts.PermissionMode
	if permMode == "" {
		permMode = c.permissionMode
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		// --verbose is mandatory for stream-json under -p (the CLI
		// rejects stream-json without it).
		"--verbose",
		"--permission-mode", permMode,
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
	if opts.EffortLevel != "" {
		args = append(args, "--effort", opts.EffortLevel)
	}
	if opts.SettingsFile != "" {
		args = append(args, "--settings", opts.SettingsFile)
	}

	// #nosec G204 -- c.cliPath comes from the trusted config file; args are
	// constructed internally (permission-mode/model/effort/session are
	// validated upstream by the slash commands before reaching here).
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
