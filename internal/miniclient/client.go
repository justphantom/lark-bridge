package miniclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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
// miniagent v3.1+ is config-only: endpoints + removed run settings
// (shell-timeout/context-window) live in the miniagent.json at ConfigPath, not
// here. ChatURL/ModelsURL/ShellTimeout/ContextWindow were deleted along with
// their CLI flags. v3.3.0 further removed ${VAR} expansion from config loading
// (c51d91c): the bridge's deploy.sh now envsubst's miniagent-cli.json at deploy
// time instead of relying on the CLI to expand placeholders.
type Config struct {
	CLIPath       string
	APIKey        string
	SystemPrompt  string
	MaxTokens     int
	MaxConcurrent int
	Stream        bool
	MaxIterations int
	Mode          string // [P2] "default"|"auto"
	Thinking      string // [P2] off|minimal|low|medium|high|xhigh|max
	KeyFile       string
	ConfigPath    string // [P4] required → -config <abspath> (config-only mode)
}

// Client wraps the miniagent binary. Safe for concurrent use: each
// Run spawns one subprocess, and a semaphore caps parallelism.
type Client struct {
	cliPath       string
	apiKey        string
	system        string
	maxTokens     int
	stream        bool
	maxIterations int
	mode          string
	thinking      string
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
		system:        cfg.SystemPrompt,
		maxTokens:     cfg.MaxTokens,
		stream:        cfg.Stream,
		maxIterations: cfg.MaxIterations,
		mode:          cfg.Mode,
		thinking:      cfg.Thinking,
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

// DefaultMaxIterations returns the configured -max-iterations default. 0 means
// unset: buildArgs then omits the flag and the upstream CLI picks its own
// default (20). Used by the miniagent handler to display the effective cap.
func (c *Client) DefaultMaxIterations() int {
	return c.maxIterations
}

// DefaultConfigPath returns the configured -config path (set at startup from
// config.ResolveConfigPath, always non-empty in production). Used by the
// miniagent handler as the global fallback when a chat has no ConfigFile pin.
func (c *Client) DefaultConfigPath() string {
	return c.configPath
}

// effectiveAPIKey returns the API key to inject as $MINIAGENT_API_KEY on the
// miniagent subprocess. KeyFile takes precedence over APIKey when set: its
// contents are read fresh each call so a rotated key file is picked up without
// a restart.
//
// miniagent removed -key-file (post-3.4.0): the key can no longer stay out of
// the subprocess env via a file-path flag. With KeyFile the bridge reads the
// file itself and still injects the value via $MINIAGENT_API_KEY, so the
// KeyFile config keeps working. Key isolation now relies on OS permissions
// (dedicated low-privilege user, 0600 config/key files) — matching miniagent's
// own README, which states shell grandchildren can read /proc/$PPID/environ.
func (c *Client) effectiveAPIKey() (string, error) {
	if c.keyFile != "" {
		b, err := os.ReadFile(c.keyFile)
		if err != nil {
			return "", fmt.Errorf("miniclient: read key_file %q: %w", c.keyFile, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return c.apiKey, nil
}

// readyTimeout bounds the `miniagent --version` health check performed by
// IsReady. Kept short so a missing/misconfigured CLI fails fast.
const readyTimeout = 10 * time.Second

// minSupportedVersion is the minimum upstream miniagent version the bridge
// requires. Bumped to 3.3.0 because: v3.2.0 deleted 9 CLI flags + multi_edit
// tool + split HTTPClient into ChatClient/StreamClient; v3.3.0 removed ${VAR}
// expansion from config loading (c51d91c) and added dual-layer .miniagent/
// rule discovery. Versions below this may emit event/tool shapes the bridge
// doesn't handle, or silently mis-load config (pre-3.3 literal ${VAR} URLs).
// The bridge special-cases "dev" (local untagged build) to always pass.
const minSupportedVersion = "3.3.0"

// DetectVersion runs `miniagent --version` and returns the parsed version
// string (e.g. "3.3.0") or "dev" for untagged builds. Returns an error only
// when the binary cannot be invoked at all.
func (c *Client) DetectVersion(ctx context.Context) (string, error) {
	if c.cliPath == "" {
		return "", fmt.Errorf("miniclient: cli_path is empty")
	}
	// #nosec G204 -- c.cliPath comes from trusted config; args are built internally.
	cmd := exec.CommandContext(ctx, c.cliPath, "--version")
	cmd.Env = cmdutil.SanitizeChildEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("miniagent --version: %w", err)
	}
	s := strings.TrimSpace(string(out))
	// "miniagent 1.2.3\n" or "miniagent dev\n" → strip the prefix.
	if idx := strings.Index(s, " "); idx >= 0 {
		s = s[idx+1:]
	}
	return strings.TrimPrefix(s, "v"), nil
}

// satisfiesVersion reports whether v is >= min using component-wise numeric
// comparison (NOT lexicographic), so 3.10.0 correctly exceeds 3.2.0. "dev"
// (untagged local build) always passes so developers are not blocked. A
// trailing pre-release suffix (e.g. "-rc1") is stripped and treated as the
// release version.
func satisfiesVersion(v, minVer string) bool {
	if v == "dev" {
		return true
	}
	return compareVersion(v, minVer) >= 0
}

// compareVersion returns -1, 0, or +1 for a<b, a==b, a>b by comparing the
// numeric dot-separated prefixes of two version strings component by
// component; a shorter version's missing components count as 0.
func compareVersion(a, b string) int {
	ra := numericVersion(a)
	rb := numericVersion(b)
	n := len(ra)
	if len(rb) > n {
		n = len(rb)
	}
	for i := range n {
		va, vb := 0, 0
		if i < len(ra) {
			va = ra[i]
		}
		if i < len(rb) {
			vb = rb[i]
		}
		switch {
		case va < vb:
			return -1
		case va > vb:
			return 1
		}
	}
	return 0
}

// numericVersion parses the leading numeric dot-separated components of a
// version string into ints, stopping at the first non-numeric component. A
// trailing pre-release suffix after '-' is dropped first: "3.10.0-rc1" →
// [3, 10, 0].
func numericVersion(s string) []int {
	if i := strings.IndexByte(s, '-'); i >= 0 {
		s = s[:i]
	}
	var out []int
	for _, part := range strings.Split(s, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}

// IsReady verifies the CLI is installed and invocable by running
// `<cliPath> --version`. Returns an error suitable for a startup health gate.
func (c *Client) IsReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	v, err := c.DetectVersion(ctx)
	if err != nil {
		return fmt.Errorf("miniagent CLI not ready: %w", err)
	}
	if !satisfiesVersion(v, minSupportedVersion) {
		return fmt.Errorf("miniagent %s does not meet minimum required version %s", v, minSupportedVersion)
	}
	return nil
}

// RunOptions describes one miniagent turn.
type RunOptions struct {
	Prompt   string
	Model    string
	Workdir  string
	Mode     string // [P2] per-chat override; "" → client default
	Thinking string // [P2] per-chat override; "" → client default
	// MaxIterations is the per-chat -max-iterations override. <=0 → client
	// default (which itself 0/unset → upstream CLI default of 20).
	MaxIterations int
	Session       string // [P3] absolute jsonl path; "" → stateless turn
	// ConfigPath is the per-chat -config override (absolute path resolved by
	// the bridge via /config). "" → client default (c.configPath, set at
	// startup from config.ResolveConfigPath).
	ConfigPath string
	Sink       io.Writer
}

// Run starts one miniagent subprocess for opts and returns the event
// stream. The caller MUST drain the channel until close. A terminal Event
// (result or error) precedes close. ctx cancellation SIGKILLs the process
// group so child tool subprocesses are reaped too.
func (c *Client) Run(ctx context.Context, opts RunOptions) (<-chan Event, error) {
	if c.cliPath == "" {
		return nil, fmt.Errorf("miniclient: cli_path is empty")
	}
	// Resolve the API key before acquiring the turn slot so a missing/unread
	// key_file fails fast instead of consuming a semaphore permit.
	apiKey, err := c.effectiveAPIKey()
	if err != nil {
		return nil, err
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
	// rides $MINIAGENT_API_KEY (the CLI has no -api-key flag); miniagent
	// removed -key-file (post-3.4.0), so even the KeyFile path is resolved by
	// the bridge and injected here — the value can no longer stay out of the
	// subprocess env.
	cmd.Env = append(cmdutil.SanitizeChildEnv(), "MINIAGENT_API_KEY="+apiKey)
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
// $MINIAGENT_API_KEY on the subprocess env (resolved in Run via
// effectiveAPIKey, which also handles the KeyFile path). miniagent v3.1+ is
// config-only: endpoints + the removed run settings (shell-timeout/context-
// window) come from the miniagent.json at -config, so buildArgs never passes
// -chat-url/-models-url/-context-window/-shell-timeout. -key-file was removed
// upstream (post-3.4.0) and is therefore never emitted. Flag form is
// single-dash to match the miniagent README.
func (c *Client) buildArgs(opts RunOptions) []string {
	a := []string{"-model", opts.Model}
	// config-only：端点/key/run 参数由 miniagent.json 解析。-config 每轮覆盖
	// （/config 命令 per-chat 切换）> client 启动默认；main.go 保证后者非空。
	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = c.configPath
	}
	a = append(a, "-config", configPath)
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
	if c.stream {
		// -stream was removed in miniagent fe85c16 and re-added in v2.0.0;
		// requires a v2.0.0+ binary (older binaries exit(2) on the unknown flag).
		a = append(a, "-stream")
	}
	// -max-iterations：每轮覆盖 > client 默认。<=0 全不传 → 上游 CLI 默认 20。
	maxIter := opts.MaxIterations
	if maxIter <= 0 {
		maxIter = c.maxIterations
	}
	if maxIter > 0 {
		a = append(a, "-max-iterations", strconv.Itoa(maxIter))
	}
	if opts.Session != "" {
		// 会话 jsonl 绝对路径（v3 -session，含 / 或 . 视为路径）。同一 chat 由
		// handler 的 startTurn busy-then-drop 串行，无并发写竞争（R4）。
		a = append(a, "-session", opts.Session)
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
