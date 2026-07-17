package peribridge

import (
	"context"
	"time"

	"github.com/hu/lark-bridge/internal/backendrpc"
	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// Handler is the peri-back orchestrator. One per process. The
// backend-agnostic spine (router, IPC client, per-chat cancel tracking,
// answer broker, emit helpers, shutdown) lives in the embedded
// bridgebase.Core; peri-back adds its CLI client, the picker option lists,
// and the settings directory. peri is stateless and emits no usage, so the
// Core's Usage store is never wired here.
type Handler struct {
	*bridgebase.Core
	agent periAPI

	// permissionOptions/effortOptions feed the interactive /perm and /effort
	// pickers. Empty falls back to built-in defaults at the call site.
	permissionOptions []string
	effortOptions     []string

	// settingsDir is scanned for the interactive /settings picker
	// (settings.json and *-settings.json). Empty → ~/.peri at runtime.
	settingsDir string
}

// HandlerConfig carries the scalar runtime config the Handler reads. It is
// populated from the config file's peri + state_dir sections by
// cmd/peri-back/main.go. PromptTimeout defaults to 0 (disabled): the CLI
// exits on its own when the turn is done, and users abort via /session-abort.
type HandlerConfig struct {
	// DefaultDirectory is reserved as the base for per-chat working dirs but
	// is currently unused: peri takes its working dir from the /cd pin or an
	// event override (see ensureBinding), never auto-derived. Retained for
	// config parity with the other bridges.
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

// NewWithLogger builds a Handler. rpc is the backend IPC client used to
// emit Control messages; logger is the main component logger.
func NewWithLogger(r *router.Router, api periAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	return &Handler{
		Core: bridgebase.NewCore(r, rpc, bridgebase.CoreConfig{
			DefaultDirectory: cfg.DefaultDirectory,
			StateDir:         cfg.StateDir,
			StreamHistory:    cfg.StreamHistory,
			PromptTimeout:    cfg.PromptTimeout,
			DebugRedact:      cfg.DebugRedact,
			WorkspaceRoot:    cfg.WorkspaceRoot,
		}, logger),
		agent:             api,
		permissionOptions: cfg.PermissionOptions,
		effortOptions:     cfg.EffortOptions,
		settingsDir:       cfg.SettingsDir,
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
