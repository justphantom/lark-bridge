package goosebridge

import (
	"context"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// Handler is the goose-back orchestrator. One per process. The
// backend-agnostic spine (router, IPC client, per-chat cancel tracking,
// answer broker, emit helpers, shutdown) lives in the embedded
// bridgebase.Core; goose-back adds its CLI client and the option lists
// feeding the interactive pickers.
type Handler struct {
	*bridgebase.Core
	agent gooseAPI

	// modelOptions/permissionOptions/effortOptions feed the interactive
	// pickers. They come from config (with defaults applied) so an operator
	// can tailor what each card offers. goose has no --permission-mode flag,
	// so permissionOptions are informational only.
	modelOptions      []string
	permissionOptions []string
	effortOptions     []string
}

// HandlerConfig carries the scalar runtime config the Handler reads. It is
// populated from the config file's goose + state_dir sections by
// cmd/goose-back/main.go. PromptTimeout defaults to 0 (disabled): the CLI
// exits on its own when the turn is done, and users abort via /session-abort.
type HandlerConfig struct {
	DefaultDirectory  string
	PermissionDefault string
	StateDir          string
	// StreamHistory caps raw stream-json captures kept under StateDir/streams.
	StreamHistory int
	// PromptTimeout is the per-prompt safety net. 0 disables it.
	PromptTimeout time.Duration
	// ModelOptions/PermissionOptions/EffortOptions feed the interactive
	// pickers. Empty falls back to built-in defaults at the call site.
	ModelOptions      []string
	PermissionOptions []string
	EffortOptions     []string
	// DebugRedact controls whether prompt/error text in debug logs is
	// replaced wholesale with <redacted>. Mirrors the top-level config field
	// log_debug_redact.
	DebugRedact bool
	// WorkspaceRoot bounds the interactive /cd picker to subdirectories of
	// this directory. Injected from the WORKSPACE_ROOT env var by main.go;
	// empty disables /cd selection (the picker surfaces a notice).
	WorkspaceRoot string
}

// NewWithLogger builds a Handler. rpc is the backend IPC client used to
// emit Control messages; logger is the main component logger.
func NewWithLogger(r *router.Router, api gooseAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	return &Handler{
		Core: bridgebase.NewCore(r, rpc, bridgebase.CoreConfig{
			DefaultDirectory:  cfg.DefaultDirectory,
			PermissionDefault: cfg.PermissionDefault,
			StateDir:          cfg.StateDir,
			StreamHistory:     cfg.StreamHistory,
			PromptTimeout:     cfg.PromptTimeout,
			DebugRedact:       cfg.DebugRedact,
			WorkspaceRoot:     cfg.WorkspaceRoot,
		}, logger),
		agent:             api,
		modelOptions:      cfg.ModelOptions,
		permissionOptions: cfg.PermissionOptions,
		effortOptions:     cfg.EffortOptions,
	}
}

// The lowercase wrappers below preserve the bridge's historical method names
// so existing call sites read unchanged; each delegates to the Core.

func (h *Handler) debugRedact() bool { return h.Core.DebugRedact() }

func (h *Handler) emit(ctx context.Context, promptID string, ctrl *protocol.Control) error {
	return h.Core.Emit(ctx, promptID, ctrl)
}

func (h *Handler) emitLogged(ctx context.Context, promptID, chatID string, ctrl *protocol.Control) {
	h.Core.EmitLogged(ctx, promptID, chatID, ctrl)
}

func (h *Handler) emitNoticeLogged(chatID, level, title, body string, extra ...string) {
	h.Core.EmitNoticeLogged(chatID, level, title, body, extra...)
}

func (h *Handler) emitAsync(promptID string, ctrl *protocol.Control) {
	h.Core.EmitAsync(promptID, ctrl)
}
