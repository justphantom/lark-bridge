package peribridge

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
)

// Handler is the peri-back orchestrator. One per process. It owns the router
// (chatID -> per-chat working directory binding), the peri client, and the
// backendrpc client used to emit Control messages. Per-chat in-flight prompts
// are tracked in cancelByChat so /session-abort and Close can cancel exactly
// one chat's run without disturbing others.
//
// Unlike the opencode bridge, there is no usage store: peri print mode emits
// no token/cost data, so there is nothing to accumulate.
type Handler struct {
	router sessionRouter
	agent  periAPI
	rpc    *backendrpc.Client
	logger *log.Logger

	// appCtx is the process-lifetime context every prompt derives from.
	appCtx    context.Context
	appCancel context.CancelFunc

	// logDebugRedact controls redaction of sensitive text in debug logs.
	logDebugRedact atomic.Bool

	// defaultDirectory is the base dir under which per-chat working
	// directories are allocated. Each chat gets defaultDirectory/<chatID>.
	// When empty, stateDir is used as the base.
	defaultDirectory string
	stateDir         string

	// permissionOptions/effortOptions feed the interactive /perm and /effort
	// pickers. Empty falls back to built-in defaults at the call site.
	permissionOptions []string
	effortOptions     []string

	// settingsDir is scanned for the interactive /settings picker
	// (settings.json and *-settings.json). Empty → ~/.peri at runtime.
	settingsDir string

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

	// pendingAnswers routes an interactive card's answer back to the goroutine
	// that emitted the Question control (/cd picker). askAndWait registers a
	// channel under the requestID, emits the card, and blocks on the channel;
	// HandleEvent's TypeAnswer branch delivers the answer. Close drains all
	// waiters so a shutdown does not leave a goroutine blocked forever.
	answerMu       sync.Mutex
	pendingAnswers map[string]chan *protocol.AnswerPayload

	// wg tracks in-flight runPrompt goroutines so Close can wait for them
	// to finish killing their subprocess before the process exits, avoiding
	// orphaned peri children.
	wg sync.WaitGroup

	closeOnce sync.Once
}

// askWaitTimeout bounds how long askAndWait blocks for a user to answer an
// interactive card. It is shorter than the frontend cardkit.InteractiveTimeout
// (10m) so the backend gives up first and surfaces a notice rather than the
// card flipping to "已失效" while the backend is still waiting.
const askWaitTimeout = 9 * time.Minute

// shutdownGrace bounds how long Close waits for in-flight prompts to wind
// down after cancelling them.
const shutdownGrace = 5 * time.Second

// HandlerConfig carries the scalar runtime config the Handler reads.
type HandlerConfig struct {
	DefaultDirectory string
	StateDir         string
	// StreamHistory caps raw NDJSON captures kept under StateDir/streams.
	StreamHistory int
	// PromptTimeout is the per-prompt safety net. 0 disables it.
	PromptTimeout time.Duration
	// PermissionOptions/EffortOptions feed the interactive pickers. Empty
	// falls back to built-in defaults at the call site.
	PermissionOptions []string
	EffortOptions     []string
	// SettingsDir is scanned for the interactive /settings picker. Empty →
	// resolve to ~/.peri at runtime via os.UserHomeDir.
	SettingsDir string
	// DebugRedact controls whether prompt/error text in debug logs is
	// replaced wholesale with <redacted>.
	DebugRedact bool
	// WorkspaceRoot bounds the interactive /cd picker to subdirectories of
	// this directory. Empty disables /cd selection.
	WorkspaceRoot string
}

// NewWithLogger builds a Handler. rpc is the backend IPC client used to emit
// Control messages; logger is the main component logger.
func NewWithLogger(r sessionRouter, api periAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	h := &Handler{
		router:            r,
		agent:             api,
		rpc:               rpc,
		logger:            logger,
		defaultDirectory:  cfg.DefaultDirectory,
		stateDir:          cfg.StateDir,
		permissionOptions: cfg.PermissionOptions,
		effortOptions:     cfg.EffortOptions,
		settingsDir:       cfg.SettingsDir,
		workspaceRoot:     cfg.WorkspaceRoot,
		streamHistory:     cfg.StreamHistory,
		promptTimeout:     cfg.PromptTimeout,
		cancelByChat:      make(map[string]*promptCancel),
		pendingAnswers:    make(map[string]chan *protocol.AnswerPayload),
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
// PromptID when the caller did not set it. A nil rpc (tests that do not wire
// an IPC client) is a no-op so the run path does not panic.
func (h *Handler) emit(ctx context.Context, promptID string, ctrl *protocol.Control) error {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	if h.rpc == nil {
		return nil
	}
	return h.rpc.SendControl(ctx, ctrl)
}

// emitLogged is emit plus a Warn on failure, for fire-and-forget callers.
func (h *Handler) emitLogged(ctx context.Context, promptID, chatID string, ctrl *protocol.Control) {
	if err := h.emit(ctx, promptID, ctrl); err != nil {
		h.logger.Warn("emit failed",
			log.FieldChatID, chatID,
			log.FieldError, err)
	}
}

// emitNoticeLogged is emitNotice plus a Warn on failure.
func (h *Handler) emitNoticeLogged(chatID, level, title, body string, extra ...string) {
	if err := h.emitNotice(chatID, level, title, body, extra...); err != nil {
		h.logger.Warn("emit notice failed",
			log.FieldChatID, chatID,
			log.FieldError, err)
	}
}

// emitSem caps the number of concurrent fire-and-forget emit goroutines.
var emitSem = make(chan struct{}, 32)

// emitAsync sends a Control in a background goroutine (fire-and-forget) so
// the stream loop never blocks on IPC latency. Each goroutine uses an
// independent 5s context — it does not inherit the prompt ctx — so an
// intermediate control still attempts delivery after the prompt is cancelled.
func (h *Handler) emitAsync(promptID string, ctrl *protocol.Control) {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	if h.rpc == nil {
		return
	}
	select {
	case emitSem <- struct{}{}:
	default:
		h.logger.Debug("emit semaphore full, dropping intermediate control",
			log.FieldControlType, ctrl.Type)
		return
	}
	goSafe(h.logger, "emit:"+string(ctrl.Type), func() {
		defer func() { <-emitSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.rpc.SendControl(ctx, ctrl); err != nil {
			h.logger.Debug("async emit failed",
				log.FieldError, err,
				log.FieldControlType, ctrl.Type)
		}
	})
}

// Close releases Handler resources. Idempotent.
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		h.appCancel()
		h.cancelAll()
		h.drainAnswers()
		h.waitPrompts()
	})
}

// waitPrompts waits for in-flight runPrompt goroutines with a bounded grace.
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
