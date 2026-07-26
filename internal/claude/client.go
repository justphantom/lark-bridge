//go:build linux || darwin

package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// readyTimeout bounds the `claude --version` health check performed by
// IsReady. Kept short so a missing/misconfigured CLI fails fast instead
// of blocking startup beyond a service manager's start timeout.
const readyTimeout = 10 * time.Second

// defaultMaxConcurrent is the fallback concurrency cap when Options does
// not supply one.
const defaultMaxConcurrent = 4

// defaultSettingsCacheTTL bounds the ListSettings cache when Options does
// not supply one.
const defaultSettingsCacheTTL = time.Hour

// runEventChanBuf is the buffer size of the per-Run Event output channel. Large
// enough that a brief consumer stall does not block the subprocess pump, small
// enough to surface backpressure quickly.
const runEventChanBuf = 64

// Options configures a Client at construction time.
type Options struct {
	// CLIPath is the claude binary to invoke. Empty defaults to "claude"
	// (PATH lookup).
	CLIPath string
	// PermissionMode is the default --permission-mode. Empty defaults to
	// "acceptEdits": the CLI's own "default" mode prompts interactively,
	// which hangs forever under -p (non-interactive) mode.
	PermissionMode string
	// AppendSystemPrompt is passed verbatim as --append-system-prompt.
	AppendSystemPrompt string
	// MaxConcurrent caps parallel subprocesses. <=0 defaults to 4.
	MaxConcurrent int
	// SettingsDir is scanned by ListSettings. Empty defaults to ~/.claude;
	// a leading "~" is expanded to $HOME.
	SettingsDir string
	// SettingsCacheTTL bounds the ListSettings cache. 0 defaults to 1h;
	// <0 disables caching (every call rescans).
	SettingsCacheTTL time.Duration
	// Logger receives debug/warn lines. nil defaults to a discard logger.
	Logger *slog.Logger
}

// Client wraps the Claude Code CLI. It is safe for concurrent use: each
// Run spawns one subprocess, and a semaphore caps the number of parallel
// subprocesses at MaxConcurrent.
type Client struct {
	cliPath            string
	permissionMode     string
	appendSystemPrompt string
	logger             *slog.Logger
	sem                chan struct{}

	// settingsDir is scanned by ListSettings for settings file variants.
	// Resolved from Options.SettingsDir at New time (empty → ~/.claude).
	// settingsTTL bounds the cache; <=0 disables caching.
	settingsDir   string
	settingsTTL   time.Duration
	settingsMu    sync.Mutex
	settingsCache *settingsListCache
}

// New builds a Client from opts, applying the documented defaults for any
// zero-valued fields (see Options).
func New(opts Options) *Client {
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	cliPath := opts.CLIPath
	if cliPath == "" {
		cliPath = "claude"
	}
	permMode := opts.PermissionMode
	if permMode == "" {
		permMode = PermissionModeAcceptEdits
	}
	n := opts.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	ttl := opts.SettingsCacheTTL
	if ttl == 0 {
		ttl = defaultSettingsCacheTTL
	}
	return &Client{
		cliPath:            cliPath,
		permissionMode:     permMode,
		appendSystemPrompt: opts.AppendSystemPrompt,
		logger:             logger,
		sem:                make(chan struct{}, n),
		settingsDir:        resolveSettingsDir(opts.SettingsDir),
		settingsTTL:        ttl,
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
	// Model optionally sets the model for this turn (--model).
	Model string
	// PermissionMode optionally overrides the Client's configured
	// --permission-mode for this turn. Empty falls back to the Client's
	// permission mode.
	PermissionMode string
	// EffortLevel optionally sets the Claude --effort level for this
	// turn. Empty falls back to Claude's default effort behavior.
	EffortLevel string
	// MaxTurns, when >0, is passed as --max-turns: the CLI aborts the
	// turn after N agent steps. Runaway/cost guard — without it a
	// misbehaving agent can loop tool calls indefinitely.
	MaxTurns int
	// AllowedTools, when non-empty, is passed verbatim as
	// --allowedTools (the CLI's own list syntax, e.g. "Bash,Read").
	AllowedTools string
	// DisallowedTools, when non-empty, is passed verbatim as
	// --disallowedTools (same list syntax).
	DisallowedTools string
	// AddDirs appends one --add-dir per entry, granting the CLI access
	// to directories outside the working directory (the CLI sandboxes
	// tool file access to cwd by default, blocking outside paths).
	AddDirs []string
	// SettingsFile optionally sets the Claude --settings file path for
	// this turn. Empty means "not set". The caller is responsible for any
	// env-var expansion before passing the path here; the client appends
	// it verbatim to the CLI args.
	SettingsFile string
	// LineSink, when non-nil, receives every raw stream-json line verbatim
	// (line + "\n") as read from stdout, before parsing. Used to archive
	// the complete CLI return stream. Writes are best-effort: errors are
	// ignored so an archive failure can never fail the run.
	LineSink io.Writer
}

// IsReady verifies the CLI is installed and invocable by running
// `<cliPath> --version`. Returns an error suitable for a startup health
// gate.
func (c *Client) IsReady(ctx context.Context) error {
	return checkVersion(ctx, c.cliPath, "claude", readyTimeout, c.logger,
		"permission_mode", c.permissionMode)
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

	cmd, err := c.buildCommand(ctx, opts)
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

	// Prompt body is intentionally NOT logged here: the caller may log a
	// redacted/truncated preview, and this client has no redact flag —
	// logging the raw prompt would leak credentials/PII users pasted in.
	c.logger.Debug("claude run started",
		"session_id", opts.SessionID,
		"dir", opts.Directory,
		"prompt_length", len(opts.Prompt),
		"model", opts.Model,
		"permission_mode", opts.PermissionMode,
		"effort_level", opts.EffortLevel,
		"settings_file", opts.SettingsFile)

	out := make(chan Event, runEventChanBuf)
	go c.pump(ctx, cmd, stdout, stderr, out, opts.LineSink)
	return out, nil
}

// buildCommand assembles the claude CLI invocation for one turn.
func (c *Client) buildCommand(ctx context.Context, opts RunOptions) (*exec.Cmd, error) {
	if c.cliPath == "" {
		return nil, errors.New("claude: cli_path is empty")
	}
	// Per-turn permission mode override; fall back to the Client's
	// configured default when the caller left it empty.
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
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	if opts.AllowedTools != "" {
		args = append(args, "--allowedTools", opts.AllowedTools)
	}
	if opts.DisallowedTools != "" {
		args = append(args, "--disallowedTools", opts.DisallowedTools)
	}
	for _, dir := range opts.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	if opts.SettingsFile != "" {
		args = append(args, "--settings", opts.SettingsFile)
	}

	// #nosec G204 -- c.cliPath comes from trusted Options; args are
	// constructed internally.
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	if opts.Directory != "" {
		cmd.Dir = opts.Directory
	}
	// Put the CLI in its own process group so cancellation can SIGKILL the
	// whole tree (the CLI spawns tool subprocesses: bash, git, npm…). Without
	// this, Kill only reaches the CLI PID and its grandchildren are orphaned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}
