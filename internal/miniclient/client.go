package miniclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/justphantom/lark-bridge/internal/log"
)

// maxLineLen caps the per-line scanner buffer for miniagent stdout.
// A single tool_result (e.g. a large file read) can be several MB.
const maxLineLen = 8 << 20 // 8 MB

// runEventChanBuf is the buffer size of the per-Run Event output channel.
const runEventChanBuf = 64

// defaultMaxConcurrent is the fallback concurrency cap.
const defaultMaxConcurrent = 4

// Config carries the scalar settings the Client reads from config.MiniAgent.
type Config struct {
	CLIPath              string
	APIKey               string
	BaseURL              string
	SystemPrompt         string
	MaxTokens            int
	Permission           string // global default permission mode
	ShellBlockedPatterns []string
	MaxConcurrent        int
}

// Client wraps the miniagent binary. Safe for concurrent use: each
// Run spawns one subprocess, and a semaphore caps parallelism.
type Client struct {
	cliPath     string
	apiKey      string
	baseURL     string
	system      string
	maxTokens   int
	permission  string // global default
	blockedPats []string
	logger      *log.Logger
	sem         chan struct{}
}

// New builds a Client. logger may be nil (→ nop).
func New(cfg Config, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Nop()
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = defaultMaxConcurrent
	}
	return &Client{
		cliPath:     cfg.CLIPath,
		apiKey:      cfg.APIKey,
		baseURL:     cfg.BaseURL,
		system:      cfg.SystemPrompt,
		maxTokens:   cfg.MaxTokens,
		permission:  cfg.Permission,
		blockedPats: cfg.ShellBlockedPatterns,
		logger:      logger,
		sem:         make(chan struct{}, n),
	}
}

// RunOptions describes one miniagent turn.
type RunOptions struct {
	Prompt     string
	Model      string
	Workdir    string
	ChatID     string
	StateDir   string
	Permission string // per-chat override; "" → use Client's global default
}

// Run starts one miniagent subprocess for opts and returns the event
// stream. The caller MUST drain the channel until close. A terminal Event
// (result or error) precedes close. ctx cancellation SIGKILLs the process
// group so child tool subprocesses are reaped too.
func (c *Client) Run(ctx context.Context, opts RunOptions) (<-chan Event, error) {
	if c.cliPath == "" {
		return nil, fmt.Errorf("miniclient: cli_path is empty")
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	args := c.buildArgs(opts)
	// #nosec G204 -- c.cliPath comes from trusted config; args are built internally.
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	cmd.Stdin = strings.NewReader(opts.Prompt)
	// Pass the API key via env, not a flag: miniagent's CLI has no --api-key
	// (passing one fails startup with "flag provided but not defined"). Inherit
	// the parent env so PATH/HOME/etc. survive, then set/override the key.
	cmd.Env = append(os.Environ(), "MINIAGENT_API_KEY="+c.apiKey)
	// Own process group so cancellation SIGKILLs the whole tree (the CLI
	// spawns tool subprocesses: bash, git…).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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
		return nil, fmt.Errorf("start miniagent: %w (is %s built?)", err, c.cliPath)
	}

	c.logger.Debug("miniagent started",
		"model", opts.Model,
		"workdir", opts.Workdir,
		"prompt_len", len(opts.Prompt))

	out := make(chan Event, runEventChanBuf)
	go c.pump(ctx, cmd, stdout, stderr, out)
	return out, nil
}

// buildArgs assembles the CLI flags from Client-level config (system prompt,
// security) + per-turn options (model, workdir, chat-id). The API key is
// intentionally NOT passed as a flag: miniagent's CLI has no --api-key
// (unknown flags fail startup), it reads $MINIAGENT_API_KEY. Run sets that
// env var explicitly on the subprocess so a backend running without the
// env in its own environment still works.
func (c *Client) buildArgs(opts RunOptions) []string {
	a := []string{
		"--model", opts.Model,
		"--verbose", // bridge always wants tool events
	}
	if c.baseURL != "" {
		a = append(a, "--base-url", c.baseURL)
	}
	if c.system != "" {
		a = append(a, "--system", c.system)
	}
	if c.maxTokens > 0 {
		a = append(a, "--max-tokens", strconv.Itoa(c.maxTokens))
	}
	if c.permission != "" {
		a = append(a, "--permission", c.permission)
	}
	if opts.Permission != "" {
		// Per-chat override replaces the global default.
		a = replaceArg(a, "--permission", opts.Permission)
	}
	if len(c.blockedPats) > 0 {
		b, _ := json.Marshal(c.blockedPats)
		a = append(a, "--blocked-patterns", string(b))
	}
	if opts.Workdir != "" {
		a = append(a, "--workdir", opts.Workdir)
	}
	if opts.ChatID != "" {
		a = append(a, "--chat-id", opts.ChatID)
	}
	if opts.StateDir != "" {
		a = append(a, "--state-dir", opts.StateDir)
	}
	return a
}

// replaceArg finds --flag <old> in args and replaces <old> with newval.
// If --flag is not present, appends --flag newval.
func replaceArg(args []string, flag, newval string) []string {
	for i := range len(args) - 1 {
		if args[i] == flag {
			args[i+1] = newval
			return args
		}
	}
	return append(args, flag, newval)
}

// pump reads stdout lines, parses them into Events, and forwards to out.
// It also tees stderr to the logger. After a terminal event (or EOF/error),
// it waits for the subprocess and closes the channel.
func (c *Client) pump(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, out chan<- Event) {
	defer func() {
		<-c.sem
		close(out)
	}()

	// Tee stderr to debug log (miniagent writes structured logs there).
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			c.logger.Debug("miniagent stderr", "line", sc.Text())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64<<10), maxLineLen)
	gotTerminal := false
	for scanner.Scan() {
		ev, ok := parseEvent(scanner.Bytes())
		if !ok {
			continue
		}
		out <- ev
		if ev.IsTerminal {
			gotTerminal = true
			break
		}
	}

	// Drain unread stdout so cmd.Wait does not block on the pipe reader.
	// Covers both the terminal-event early break and a cancel mid-stream.
	go func() {
		_, _ = io.Copy(io.Discard, stdout)
	}()

	// Group-kill on ctx cancellation: exec.CommandContext only SIGKILLs the
	// main process (miniagent), not its children. With Setpgid=true the
	// whole tree shares a process group id = cmd.Process.Pid. Sending
	// syscall.Kill(-pgid, SIGKILL) reaches sh → git → node etc. so abort
	// does not leave orphaned tool subprocesses holding locks or ports.
	if ctx.Err() != nil && cmd.Process != nil {
		pgid := cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		// Non-zero exit without ctx cancel: if we already emitted a terminal
		// event, the consumer has it and this is just the exit code reflecting
		// an error-path result. If we did NOT (process crashed before writing
		// one), synthesize one so the consumer's drain-loop terminates.
		if !gotTerminal {
			out <- Event{Kind: KindError, Message: fmt.Sprintf("miniagent exited: %v", err), IsError: true, IsTerminal: true}
		}
	}
}
