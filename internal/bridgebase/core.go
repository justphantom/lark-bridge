package bridgebase

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
	"github.com/hu/lark-bridge/internal/usage"
)

const (
	// shutdownGrace bounds how long Close waits for in-flight prompts to wind
	// down after cancelling them. It is long enough to SIGKILL the subprocess
	// group and reap it, short enough that a stuck goroutine does not hang the
	// process on SIGTERM.
	shutdownGrace = 5 * time.Second

	// emitConcurrency caps the number of concurrent fire-and-forget emit
	// goroutines (see EmitAsync).
	emitConcurrency = 32
)

// PromptCancel is the cancel entry of one in-flight prompt, registered under
// its chatID so /session-abort and Close can cancel exactly one chat's run
// without disturbing others.
type PromptCancel struct {
	Cancel    context.CancelFunc
	StartTime time.Time
	ChatID    string
}

// CoreConfig carries the scalar runtime config every bridge's Handler reads,
// populated from the config file by each backend's main.go. PromptTimeout
// defaults to 0 (disabled): the CLI exits on its own when the turn is done,
// and users abort via /session-abort.
type CoreConfig struct {
	DefaultDirectory string
	// PermissionDefault is the process-level permission mode. A per-chat pin
	// overrides it; an empty pin falls back to this. Backends without a
	// permission concept leave it unset (informational only in /current).
	PermissionDefault string
	StateDir          string
	// StreamHistory caps raw stream-json captures kept under StateDir/streams.
	StreamHistory int
	// PromptTimeout is the per-prompt safety net. 0 disables it.
	PromptTimeout time.Duration
	// DebugRedact controls whether prompt/error text in debug logs is
	// replaced wholesale with <redacted>. Mirrors the top-level config field
	// log_debug_redact.
	DebugRedact bool
	// WorkspaceRoot bounds the interactive /cd picker to subdirectories of
	// this directory. Injected from the WORKSPACE_ROOT env var by main.go;
	// empty disables /cd selection (the picker surfaces a notice).
	WorkspaceRoot string
}

// Core is the backend-agnostic spine every bridge's Handler embeds: the
// router, the IPC client, per-chat cancel tracking, the answer broker, the
// emit helpers, and shutdown. Bridge code keeps only its agent client and
// option lists on top.
type Core struct {
	Router *router.Router
	RPC    *backendrpc.Client
	Logger *log.Logger

	// AppCtx is the process-lifetime context every prompt derives from.
	AppCtx    context.Context
	AppCancel context.CancelFunc

	logDebugRedact atomic.Bool

	// DefaultDirectory is the base dir under which per-chat working
	// directories are allocated. Each chat gets DefaultDirectory/<chatID>.
	DefaultDirectory string

	// StateDir is the base dir for persistent state.
	StateDir string

	// StreamHistory caps how many raw stream-json captures are kept under
	// {StateDir}/streams. <=0 disables archiving.
	StreamHistory int

	// PermissionDefault mirrors CoreConfig.PermissionDefault.
	PermissionDefault string

	// DirCache bounds /cd to subdirectories of the configured workspace root
	// (empty root disables the picker) and caches the scan.
	DirCache *DirCache

	// PromptTimeout is the per-prompt safety net. 0 disables it (the CLI
	// exits on its own). When >0, a prompt exceeding this duration is
	// cancelled so a stuck CLI cannot occupy a slot forever.
	PromptTimeout time.Duration

	// CancelByChat maps chatID to the cancel entry of the runPrompt goroutine
	// currently working on it. Busy-then-drop: a chat with an entry is busy
	// and new prompts are rejected with a heads-up notice.
	CancelMu     sync.Mutex
	CancelByChat map[string]*PromptCancel

	// Wg tracks in-flight runPrompt goroutines so Close can wait for them to
	// finish killing their subprocess before the process exits, avoiding
	// orphaned CLI children.
	Wg sync.WaitGroup

	// Answers routes an interactive card's answer back to the goroutine that
	// emitted the Question control. Close drains all waiters so a shutdown
	// does not leave a goroutine blocked forever.
	Answers *AnswerBroker

	// emitSem caps concurrent fire-and-forget emit goroutines so an extreme
	// event burst cannot exhaust goroutines (see EmitAsync). A Core field
	// (not a package global) so concurrent tests do not share one semaphore.
	emitSem chan struct{}

	// Usage records per-session token/cost totals. nil when not wired (e.g.
	// unit tests).
	Usage *usage.Store

	closeOnce sync.Once
}

// NewCore builds the Core. rpc is the backend IPC client used to emit Control
// messages; logger is the main component logger (nil → no-op).
func NewCore(r *router.Router, rpc *backendrpc.Client, cfg CoreConfig, logger *log.Logger) *Core {
	if logger == nil {
		logger = log.Nop()
	}
	c := &Core{
		Router:            r,
		RPC:               rpc,
		Logger:            logger,
		DefaultDirectory:  cfg.DefaultDirectory,
		PermissionDefault: cfg.PermissionDefault,
		StateDir:          cfg.StateDir,
		StreamHistory:     cfg.StreamHistory,
		PromptTimeout:     cfg.PromptTimeout,
		DirCache:          NewDirCache(cfg.WorkspaceRoot),
		CancelByChat:      make(map[string]*PromptCancel),
		Answers:           NewAnswerBroker(),
		emitSem:           make(chan struct{}, emitConcurrency),
	}
	c.AppCtx, c.AppCancel = context.WithCancel(context.Background())
	c.logDebugRedact.Store(cfg.DebugRedact)
	return c
}

// DebugRedact reports the current debug-redact flag.
func (c *Core) DebugRedact() bool {
	return c.logDebugRedact.Load()
}

// Emit sends a Control to the frontend via the IPC client, back-filling
// PromptID when the caller did not set it. A nil rpc (tests that do not wire
// an IPC client) is a no-op so the run path does not panic.
func (c *Core) Emit(ctx context.Context, promptID string, ctrl *protocol.Control) error {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	if c.RPC == nil {
		return nil
	}
	return c.RPC.SendControl(ctx, ctrl)
}

// EmitLogged is Emit plus a Warn on failure, for fire-and-forget callers that
// previously discarded the error silently. chatID is recorded for triage; it
// may differ from promptID when promptID is a reply target.
func (c *Core) EmitLogged(ctx context.Context, promptID, chatID string, ctrl *protocol.Control) {
	if err := c.Emit(ctx, promptID, ctrl); err != nil {
		c.Logger.Warn("emit failed",
			log.FieldChatID, chatID,
			log.FieldError, err)
	}
}

// EmitNoticeLogged is EmitNotice plus a Warn on failure, for fire-and-forget
// callers that previously discarded the error silently.
func (c *Core) EmitNoticeLogged(chatID, level, title, body string, extra ...string) {
	if err := EmitNotice(c.AppCtx, c.Emit, chatID, level, title, body, extra...); err != nil {
		c.Logger.Warn("emit notice failed",
			log.FieldChatID, chatID,
			log.FieldError, err)
	}
}

// EmitAsync sends a Control in a background goroutine (fire-and-forget) so
// the stream loop never blocks on IPC latency. Each goroutine uses an
// independent 5s context (not the prompt ctx); intermediate controls are
// disposable — the terminal control goes through synchronous Emit (emitTerminal).
func (c *Core) EmitAsync(promptID string, ctrl *protocol.Control) {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	if c.RPC == nil {
		return
	}
	select {
	case c.emitSem <- struct{}{}:
	default:
		// Semaphore full (32 in-flight emits + slow IPC): drop a disposable
		// intermediate control rather than block the stream loop. The terminal
		// control always goes through the synchronous emit (emitTerminal), so
		// this never loses the final card.
		c.Logger.Debug("emit semaphore full, dropping intermediate control",
			log.FieldControlType, ctrl.Type)
		return
	}
	GoSafe(c.Logger, "emit:"+string(ctrl.Type), func() {
		defer func() { <-c.emitSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.RPC.SendControl(ctx, ctrl); err != nil {
			c.Logger.Debug("async emit failed",
				log.FieldError, err,
				log.FieldControlType, ctrl.Type)
		}
	})
}

// Close releases Core resources. Idempotent: cancels AppCtx (aborting
// in-flight prompts) and per-chat cancels, waits up to shutdownGrace for
// runPrompt goroutines so subprocesses are reaped, not orphaned.
func (c *Core) Close() {
	c.closeOnce.Do(func() {
		c.AppCancel()
		c.CancelAll()
		c.Answers.Drain()
		c.WaitPrompts()
		if c.Usage != nil {
			c.Usage.Close()
		}
	})
}

// SetUsage wires the per-session usage store. Called by main.go after the
// constructor; nil is a no-op so tests that do not wire it are unaffected.
func (c *Core) SetUsage(s *usage.Store) {
	if s != nil {
		c.Usage = s
	}
}

// WaitPrompts waits for in-flight runPrompt goroutines with a bounded grace
// period; a stuck goroutine cannot hang shutdown.
func (c *Core) WaitPrompts() {
	done := make(chan struct{})
	go func() {
		c.Wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
}

// CancelAll cancels every registered per-chat prompt.
func (c *Core) CancelAll() {
	c.CancelMu.Lock()
	defer c.CancelMu.Unlock()
	for _, pc := range c.CancelByChat {
		pc.Cancel()
	}
}
