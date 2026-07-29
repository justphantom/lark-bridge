package ompbridge

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// Handler is the omp-back orchestrator. One per process. The
// backend-agnostic spine (router, IPC client, per-chat cancel tracking,
// answer broker, emit helpers, shutdown) lives in the embedded
// bridgebase.Core; omp-back adds its CLI client plus the option lists
// feeding the interactive pickers (model/approval/thinking — omp has no
// agent concept, unlike opencode).
type Handler struct {
	*bridgebase.Core

	agent ompAPI

	// thinkingDefault is the fallback --thinking value when a chat's
	// binding.EffortLevel pin is empty. CoreConfig has no equivalent field
	// (permission has one: PermissionDefault), so it lives here on Handler.
	// Mirrors how claudeback carries effortOptions alongside the Core.
	thinkingDefault string
	// modelOptions/approvalOptions/thinkingOptions feed the interactive
	// pickers. They come from config (with defaults applied) so an operator
	// can tailor what each card offers.
	modelOptions    []string
	approvalOptions []string
	thinkingOptions []string
}

// HandlerConfig carries the scalar runtime config the Handler reads. It is
// populated from the config file's omp + state_dir sections by
// cmd/omp-back/main.go. PromptTimeout defaults to 0 (disabled): the CLI
// exits on its own when the turn is done, and users abort via /session-abort.
type HandlerConfig struct {
	DefaultDirectory string
	StateDir         string
	// StreamHistory caps raw NDJSON captures kept under StateDir/streams.
	StreamHistory int
	// PromptTimeout is the per-prompt safety net. 0 disables it.
	PromptTimeout time.Duration
	// IdleTimeout is the per-prompt idle watchdog: cancel the subprocess
	// (SIGKILL the group) when no stdout event arrives for this duration.
	// 0 disables it. Wired from config Timeouts.IdleTimeout.
	IdleTimeout time.Duration
	// DebugRedact controls whether prompt/error text in debug logs is
	// replaced wholesale with <redacted>. Mirrors the top-level config field
	// log_debug_redact.
	DebugRedact bool
	// WorkspaceRoot bounds the interactive /cd picker to subdirectories of
	// this directory. Injected from the WORKSPACE_ROOT env var by main.go;
	// empty disables /cd selection (the picker surfaces a notice).
	WorkspaceRoot string
	// ApprovalDefault is the process-level --approval-mode fallback (always-
	// ask|write|yolo). A per-chat pin overrides it; an empty pin falls back
	// to this. Mapped onto CoreConfig.PermissionDefault (semantic equivalent
	// of claude's permission_mode) so /current and the Core's display path
	// work without ompbridge-specific plumbing.
	ApprovalDefault string
	// ThinkingDefault is the process-level --thinking fallback. Used only
	// when a chat's effort pin is empty (CoreConfig has no field for it).
	ThinkingDefault string
	// ModelOptions/ApprovalOptions/ThinkingOptions feed the interactive
	// pickers. Empty falls back to built-in defaults at the call site.
	ModelOptions    []string
	ApprovalOptions []string
	ThinkingOptions []string
}

// NewWithLogger builds a Handler. rpc is the backend IPC client used to emit
// Control messages; logger is the main component logger.
func NewWithLogger(r *router.Router, api ompAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	return &Handler{
		Core: bridgebase.NewCore(r, rpc, bridgebase.CoreConfig{
			DefaultDirectory:  cfg.DefaultDirectory,
			PermissionDefault: cfg.ApprovalDefault,
			StateDir:          cfg.StateDir,
			StreamHistory:     cfg.StreamHistory,
			PromptTimeout:     cfg.PromptTimeout,
			IdleTimeout:       cfg.IdleTimeout,
			DebugRedact:       cfg.DebugRedact,
			WorkspaceRoot:     cfg.WorkspaceRoot,
		}, logger),
		agent:            api,
		thinkingDefault:  cfg.ThinkingDefault,
		modelOptions:     cfg.ModelOptions,
		approvalOptions:  cfg.ApprovalOptions,
		thinkingOptions:  cfg.ThinkingOptions,
	}
}

// The lowercase wrappers below preserve the bridge's historical method names
// so existing call sites read unchanged; each delegates to the Core.

func (h *Handler) debugRedact() bool { return h.DebugRedact() }

func (h *Handler) emit(ctx context.Context, promptID string, ctrl *protocol.Control) error {
	return h.Emit(ctx, promptID, ctrl)
}

func (h *Handler) emitLogged(ctx context.Context, promptID, chatID string, ctrl *protocol.Control) {
	h.EmitLogged(ctx, promptID, chatID, ctrl)
}

func (h *Handler) emitCardUpdateLogged(chatID, messageID, level, title, body string, extra ...string) {
	h.EmitCardUpdateLogged(chatID, messageID, level, title, body, extra...)
}

// emitPromptNotice delegates to Core.EmitPromptNotice (shared across bridges).
func (h *Handler) emitPromptNotice(chatID, promptID, level, title, body string) {
	h.EmitPromptNotice(chatID, promptID, level, title, body)
}

func (h *Handler) emitAsync(promptID string, ctrl *protocol.Control) {
	h.EmitAsync(promptID, ctrl)
}
