package opencodebridge

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/usage"
)

// Handler is the opencode-back orchestrator. One per process. It owns the
// router (chatID -> opencode session binding), the opencode client, and the
// backendrpc client used to emit Control messages. Per-chat in-flight prompts
// are tracked in cancelByChat so /session-abort and Close can cancel exactly
// one chat's run without disturbing others.
type Handler struct {
	router sessionRouter
	agent  opencodeAPI
	rpc    *backendrpc.Client
	logger *log.Logger

	// appCtx is the process-lifetime context every prompt derives from.
	appCtx    context.Context
	appCancel context.CancelFunc

	// logDebugRedact controls redaction of sensitive text in debug logs.
	logDebugRedact atomic.Bool

	// defaultDirectory is reserved as the base for per-chat working dirs but is
	// currently unused: opencode takes its working dir from the /cd pin or an
	// event override (see ensureBinding), never auto-derived. Retained for
	// config parity with the claude bridge.
	defaultDirectory string
	stateDir         string

	// workspaceRoot bounds /cd to its subdirectories. Empty disables the
	// picker. workspaceMu/workspaceCache memoise the one-level subdir scan
	// for workspaceCacheTTL so repeated /cd pickers are instant.
	workspaceRoot  string
	workspaceMu    sync.Mutex
	workspaceCache *dirListCache

	// streamHistory caps how many raw NDJSON captures are kept under
	// {stateDir}/streams. <=0 disables archiving.
	streamHistory int

	// promptTimeout is the per-prompt safety net. 0 disables it (the CLI
	// exits on its own). When >0, a prompt exceeding this duration is
	// cancelled so a stuck CLI cannot occupy a slot forever.
	promptTimeout time.Duration

	// cancelByChat maps chatID to the cancel entry of the runPrompt
	// goroutine currently working on it. Busy-then-drop: a chat with an
	// entry is busy and new prompts are rejected with a heads-up notice.
	cancelMu     sync.Mutex
	cancelByChat map[string]*promptCancel

	// wg tracks in-flight runPrompt goroutines so Close can wait for them
	// to finish killing their subprocess before the process exits, avoiding
	// orphaned claude/opencode children.
	wg sync.WaitGroup

	// answers routes an interactive card's answer back to the goroutine that
	// emitted the Question control. askAndWait registers under the requestID,
	// emits the card, and blocks on the channel; HandleEvent's TypeAnswer
	// branch delivers the answer. Close drains all waiters so a shutdown does
	// not leave a goroutine blocked forever. Implemented by bridgebase so the
	// three bridges share one routing implementation instead of a per-bridge
	// copy.
	answers *bridgebase.AnswerBroker

	// emitSem caps concurrent fire-and-forget emit goroutines so an extreme
	// event burst cannot exhaust goroutines (see emitAsync). A Handler field
	// (not a package global) so concurrent tests do not share one semaphore.
	emitSem chan struct{}

	// usage records per-session token/cost totals. nil when not wired
	// (e.g. unit tests). Set via SetUsage before the first prompt.
	usage *usage.Store

	closeOnce sync.Once
}

// askWaitTimeout bounds how long askAndWait blocks for a user to answer an
// interactive card. It is shorter than the frontend cardkit.InteractiveTimeout
// (10m) so the backend gives up first and surfaces a notice rather than the
// card flipping to "已失效" while the backend is still waiting.
const askWaitTimeout = 9 * time.Minute

// shutdownGrace bounds how long Close waits for in-flight prompts to wind
// down after cancelling them. It is long enough to SIGKILL the subprocess
// group and reap it, short enough that a stuck goroutine does not hang the
// process on SIGTERM.
const shutdownGrace = 5 * time.Second

// emitConcurrency caps the number of concurrent fire-and-forget emit
// goroutines (see emitAsync).
const emitConcurrency = 32

// HandlerConfig carries the scalar runtime config the Handler reads. It is
// populated from the config file's opencode + state_dir sections by
// cmd/opencode-back/main.go. PromptTimeout defaults to 0 (disabled):
// opencode agent tasks can run many tool-call rounds, and users abort via
// /session-abort.
type HandlerConfig struct {
	DefaultDirectory string
	StateDir         string
	// StreamHistory caps raw NDJSON captures kept under StateDir/streams.
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

// NewWithLogger builds a Handler. rpc is the backend IPC client used to emit
// Control messages; logger is the main component logger.
func NewWithLogger(r sessionRouter, api opencodeAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	h := &Handler{
		router:           r,
		agent:            api,
		rpc:              rpc,
		logger:           logger,
		defaultDirectory: cfg.DefaultDirectory,
		stateDir:         cfg.StateDir,
		workspaceRoot:    cfg.WorkspaceRoot,
		streamHistory:    cfg.StreamHistory,
		promptTimeout:    cfg.PromptTimeout,
		cancelByChat:     make(map[string]*promptCancel),
		answers:          bridgebase.NewAnswerBroker(),
		emitSem:          make(chan struct{}, emitConcurrency),
	}
	h.appCtx, h.appCancel = context.WithCancel(context.Background())
	h.logDebugRedact.Store(cfg.DebugRedact)
	return h
}

// debugRedact reports the current debug-redact flag.
func (h *Handler) debugRedact() bool {
	return h.logDebugRedact.Load()
}

// emit sends a Control to the frontend via the IPC client, back-filling
// PromptID when the caller did not set it. All former emit call sites route
// through here. A nil rpc (tests that do not wire an IPC client) is a no-op
// so the run path does not panic.
func (h *Handler) emit(ctx context.Context, promptID string, ctrl *protocol.Control) error {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	if h.rpc == nil {
		return nil
	}
	return h.rpc.SendControl(ctx, ctrl)
}

// emitLogged is emit plus a Warn on failure, for fire-and-forget callers
// that previously discarded the error silently. chatID is recorded for
// triage; it may differ from promptID when promptID is a reply target.
func (h *Handler) emitLogged(ctx context.Context, promptID, chatID string, ctrl *protocol.Control) {
	if err := h.emit(ctx, promptID, ctrl); err != nil {
		h.logger.Warn("emit failed",
			log.FieldChatID, chatID,
			log.FieldError, err)
	}
}

// emitNoticeLogged is emitNotice plus a Warn on failure, for fire-and-forget
// callers that previously discarded the error silently.
func (h *Handler) emitNoticeLogged(chatID, level, title, body string, extra ...string) {
	if err := h.emitNotice(chatID, level, title, body, extra...); err != nil {
		h.logger.Warn("emit notice failed",
			log.FieldChatID, chatID,
			log.FieldError, err)
	}
}

// emitAsync sends a Control in a background goroutine (fire-and-forget) so
// the stream loop never blocks on IPC latency. Each goroutine uses an
// independent 5s context — it does not inherit the prompt ctx — so an
// intermediate control still attempts delivery after the prompt is cancelled
// (it will likely fail, which is acceptable: intermediate controls are
// disposable; the terminal control is the one that matters and goes through
// the synchronous emit path in emitTerminal).
func (h *Handler) emitAsync(promptID string, ctrl *protocol.Control) {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	if h.rpc == nil {
		return
	}
	select {
	case h.emitSem <- struct{}{}:
	default:
		// Semaphore full (32 in-flight emits + slow IPC): drop a disposable
		// intermediate control rather than block the stream loop. The terminal
		// control always goes through the synchronous emit (emitTerminal), so
		// this never loses the final card.
		h.logger.Debug("emit semaphore full, dropping intermediate control",
			log.FieldControlType, ctrl.Type)
		return
	}
	bridgebase.GoSafe(h.logger, "emit:"+string(ctrl.Type), func() {
		defer func() { <-h.emitSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.rpc.SendControl(ctx, ctrl); err != nil {
			h.logger.Debug("async emit failed",
				log.FieldError, err,
				log.FieldControlType, ctrl.Type)
		}
	})
}

// Close releases Handler resources. Idempotent. It cancels the process-lifetime
// appCtx (aborting every in-flight prompt) and every per-chat cancel, then
// waits up to shutdownGrace for the runPrompt goroutines to finish so their
// subprocesses are reaped rather than orphaned.
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		h.appCancel()
		h.cancelAll()
		h.drainAnswers()
		h.waitPrompts()
		if h.usage != nil {
			h.usage.Close()
		}
	})
}

// SetUsage wires the per-session usage store. Called by main.go after
// NewWithLogger; nil is a no-op so tests that do not wire it are unaffected.
func (h *Handler) SetUsage(s *usage.Store) {
	if s != nil {
		h.usage = s
	}
}

// waitPrompts waits for in-flight runPrompt goroutines with a bounded grace
// period so a stuck goroutine cannot hang shutdown.
func (h *Handler) waitPrompts() {
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
}

// cancelAll cancels every registered per-chat prompt.
func (h *Handler) cancelAll() {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	for _, pc := range h.cancelByChat {
		pc.cancel()
	}
}
