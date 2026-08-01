package miniclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase/linereader"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
)

// maxLineLen caps the per-line scanner buffer for miniagent stdout.
// A single tool_use input (e.g. a large file write) can be several MB.
const maxLineLen = 8 << 20 // 8 MB

// runEventChanBuf is the buffer size of the per-Run Event output channel.
const runEventChanBuf = 64

// maxStderrBytes bounds the stderr capture so a pathological miniagent run
// cannot exhaust memory. The head of stderr holds the actionable diagnostic
// (missing API key, bad model, panic stack); 64 KiB is ample for that.
const maxStderrBytes = 64 << 10

// defaultMaxConcurrent is the fallback concurrency cap.
const defaultMaxConcurrent = 4

// Config carries the scalar settings the Client reads from config.MiniAgent.
type Config struct {
	CLIPath       string
	APIKey        string
	ChatURL       string // [P1] full chat completions URL (bare mode required)
	ModelsURL     string // [P1] full models URL (bare mode, /models)
	SystemPrompt  string
	MaxTokens     int
	MaxConcurrent int
	Stream        bool
	MaxIterations int
	ShellTimeout  time.Duration
	Mode          string // [P2] "default"|"auto"
	Thinking      string // [P2] off|minimal|low|medium|high|xhigh|max
	ContextWindow int    // [P2] >0 enables compaction
	KeyFile       string
	ConfigPath    string // [P4] non-empty → config mode (no chat/models-url)
}

// Client wraps the miniagent binary. Safe for concurrent use: each
// Run spawns one subprocess, and a semaphore caps parallelism.
type Client struct {
	cliPath       string
	apiKey        string
	chatURL       string
	modelsURL     string
	system        string
	maxTokens     int
	stream        bool
	maxIterations int
	shellTimeout  time.Duration
	mode          string
	thinking      string
	contextWindow int
	keyFile       string
	configPath    string
	logger        *log.Logger
	sem           chan struct{}
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
		cliPath:       cfg.CLIPath,
		apiKey:        cfg.APIKey,
		chatURL:       cfg.ChatURL,
		modelsURL:     cfg.ModelsURL,
		system:        cfg.SystemPrompt,
		maxTokens:     cfg.MaxTokens,
		stream:        cfg.Stream,
		maxIterations: cfg.MaxIterations,
		shellTimeout:  cfg.ShellTimeout,
		mode:          cfg.Mode,
		thinking:      cfg.Thinking,
		contextWindow: cfg.ContextWindow,
		keyFile:       cfg.KeyFile,
		configPath:    cfg.ConfigPath,
		logger:        logger,
		sem:           make(chan struct{}, n),
	}
}

// DefaultMode returns the configured -mode default ("default" when unset).
func (c *Client) DefaultMode() string {
	if c.mode == "" {
		return "default"
	}
	return c.mode
}

// DefaultThinking returns the configured -thinking default ("off" when unset).
func (c *Client) DefaultThinking() string {
	if c.thinking == "" {
		return "off"
	}
	return c.thinking
}

// RunOptions describes one miniagent turn.
type RunOptions struct {
	Prompt   string
	Model    string
	Workdir  string
	Mode     string // [P2] per-chat override; "" → client default
	Thinking string // [P2] per-chat override; "" → client default
	Session  string // [P3] absolute jsonl path; "" → stateless turn
	Sink     io.Writer
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
	// API key injection. Inherit a SANITISED parent env (bridge's own secrets
	// — FEISHU_APP_SECRET / IPC_SECRET / ENCRYPT_KEY … — are stripped so a
	// user-run shell tool inside miniagent cannot read them). The key itself
	// is passed via $MINIAGENT_API_KEY (the CLI has no -api-key flag) UNLESS
	// KeyFile is set: then buildArgs already added -key-file (a path, not the
	// key), and the key must NOT also enter the env — that would defeat the
	// file-based path's whole purpose (keeping the key out of the process env
	// so /proc/$PPID/environ can't leak it to a shell grandchild).
	if c.keyFile == "" {
		cmd.Env = append(cmdutil.SanitizeChildEnv(), "MINIAGENT_API_KEY="+c.apiKey)
	} else {
		cmd.Env = cmdutil.SanitizeChildEnv()
	}
	// Tree-wide SIGKILL on ctx cancel: the CLI spawns tool subprocesses
	// (bash, git …) that inherit the stdout pipe write end. Without a
	// process group + WaitDelay, those grandchildren keep the scanner
	// blocked after the main process dies.
	cmdutil.ApplyGroupCancel(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		<-c.sem
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		<-c.sem
		// Close the already-open stdout pipe: with Start never called,
		// exec never gets to close it (P4 fd leak).
		_ = stdout.Close()
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
	go c.pump(ctx, cmd, stdout, stderr, out, opts.Sink)
	return out, nil
}

// buildArgs assembles miniagent CLI flags. The API key is NOT a flag: it rides
// $MINIAGENT_API_KEY on the subprocess env (set in Run) unless KeyFile is set
// (then -key-file points at a path). v3.0.0 removed -base-url/-confine: bare
// mode uses -chat-url (and -models-url for listing); config mode uses -config.
// Flag form is single-dash to match the miniagent README.
func (c *Client) buildArgs(opts RunOptions) []string {
	a := []string{"-model", opts.Model}
	if c.configPath != "" {
		// config 模式：端点/key 由 miniagent.json 解析，不传 chat/models-url。
		a = append(a, "-config", c.configPath)
	} else if c.chatURL != "" {
		a = append(a, "-chat-url", c.chatURL)
	}
	if c.system != "" {
		a = append(a, "-system", c.system)
	}
	if c.maxTokens > 0 {
		a = append(a, "-max-tokens", strconv.Itoa(c.maxTokens))
	}
	if opts.Workdir != "" {
		a = append(a, "-workdir", opts.Workdir)
	}
	// -mode（替代 v2 -confine）：每轮覆盖 > client 默认。default 模式需 workdir，
	// 由 main.go 强校验 WorkspaceRoot + /cd 仅设非空保证。
	mode := opts.Mode
	if mode == "" {
		mode = c.mode
	}
	if mode != "" {
		a = append(a, "-mode", mode)
	}
	// -thinking：每轮覆盖 > client 默认。
	thinking := opts.Thinking
	if thinking == "" {
		thinking = c.thinking
	}
	if thinking != "" {
		a = append(a, "-thinking", thinking)
	}
	if c.contextWindow > 0 {
		a = append(a, "-context-window", strconv.Itoa(c.contextWindow))
	}
	if c.stream {
		// -stream was removed in miniagent fe85c16 and re-added in v2.0.0;
		// requires a v2.0.0+ binary (older binaries exit(2) on the unknown flag).
		a = append(a, "-stream")
	}
	if c.maxIterations > 0 {
		a = append(a, "-max-iterations", strconv.Itoa(c.maxIterations))
	}
	if c.shellTimeout > 0 {
		a = append(a, "-shell-timeout", c.shellTimeout.String())
	}
	if opts.Session != "" {
		// 会话 jsonl 绝对路径（v3 -session，含 / 或 . 视为路径）。同一 chat 由
		// handler 的 startTurn busy-then-drop 串行，无并发写竞争（R4）。
		a = append(a, "-session", opts.Session)
	}
	if c.keyFile != "" {
		a = append(a, "-key-file", c.keyFile)
	}
	return a
}

// pump reads stdout lines, parses them into Events, and forwards to out. It
// captures stderr into a bounded buffer (for the error-event message) while
// teeing each line to the debug log. After a terminal event (or EOF/error) it
// waits — with stderr fully drained first — for the subprocess and closes the
// channel.
//
// Alignment with the claude/opencode/omp pump (P1B): (1) the stderr copier is
// synced to cmd.Wait via stderrDone so the os/exec pipe contract holds (no
// racing a Wait that may close the read end); (2) the captured stderr is
// appended to the synthesized error event so a startup failure ("missing API
// key", panic stack) reaches the user instead of vanishing; (3) the
// fire-and-forget stdout drain goroutine is removed — ApplyGroupCancel's
// WaitDelay (set in Run) SIGKILLs the process group and bounds Wait so a
// stuck grandchild cannot keep the stdout pipe open forever.
func (c *Client) pump(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.Reader, out chan<- Event, sink io.Writer) {
	defer func() {
		<-c.sem
		close(out)
	}()

	// Capture stderr into a bounded buffer for the error-event message AND tee
	// each line to the debug log. Bounded by maxStderrBytes so a misbehaving
	// miniagent cannot OOM us. stderrDone syncs the copier to cmd.Wait below.
	var stderrBuf bytes.Buffer
	stderrDone := make(chan struct{})
	go func() {
		tee := io.TeeReader(io.LimitReader(stderr, maxStderrBytes), &stderrBuf)
		sc := bufio.NewScanner(tee)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			c.logger.Debug("miniagent stderr", "line", sc.Text())
		}
		// Drain anything past the LimitReader cap so the subprocess is not
		// blocked writing stderr (it would then never exit). Discard silently.
		_, _ = io.Copy(io.Discard, stderr)
		close(stderrDone)
	}()

	lr := linereader.New(stdout, maxLineLen)
	gotTerminal := false
readLoop:
	for {
		line, truncated, err := lr.ReadLine()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			c.logger.Error("miniagent stdout read error", "error", err)
			break
		}

		if truncated {
			eventmetrics.LineTruncated("miniagent").Inc()
			c.logger.Warn("miniagent stream line truncated", "kept_len", len(line), "max", maxLineLen)
		}

		if sink != nil {
			_, _ = io.WriteString(sink, line+"\n") //nolint:gosec // G705: sink is a streamarchive file writer, not an HTTP response
		}

		ev, ok := parseEvent([]byte(line))
		if !ok {
			continue
		}
		// out is buffered but bounded; a consumer that stops draining without
		// cancelling ctx would otherwise block this send (and the whole pump
		// goroutine) forever. Abandon the rest on ctx cancel — the deferred
		// close(out) and cmd teardown below still run (Low#16).
		select {
		case out <- ev:
		case <-ctx.Done():
			break readLoop
		}
		if ev.IsTerminal {
			gotTerminal = true
			break
		}
	}

	// Wait for the stderr copier to finish before cmd.Wait: Wait may close
	// the pipe read end, and a concurrent copy would race it (os/exec
	// contract: the caller must not use StderrPipe after Wait).
	<-stderrDone

	if err := cmd.Wait(); err != nil && ctx.Err() == nil {
		// Non-zero exit without ctx cancel: if we already emitted a terminal
		// event, the consumer has it and this is just the exit code reflecting
		// an error-path result. If we did NOT (process crashed before writing
		// one), synthesize one so the consumer's drain-loop terminates — and
		// fold the captured stderr in so the actionable diagnostic surfaces.
		if !gotTerminal {
			msg := fmt.Sprintf("miniagent exited: %v", err)
			if s := strings.TrimSpace(stderrBuf.String()); s != "" {
				msg += "; stderr: " + s
			}
			out <- Event{Kind: KindError, Message: msg, IsTerminal: true}
		}
	}
}
