package claudebridge

import (
	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// Handler is the claude-back orchestrator. One per process. The
// backend-agnostic spine (router, IPC client, per-chat cancel tracking,
// answer broker, emit helpers, shutdown) lives in the embedded
// bridgebase.Core; claude-back adds its CLI client and the option lists
// feeding the interactive pickers.
type Handler struct {
	*bridgebase.Core

	agent claudeAPI

	// modelOptions/permissionOptions/effortOptions feed the interactive
	// pickers. They come from config (with defaults applied) so an operator
	// can tailor what each card offers.
	modelOptions      []string
	permissionOptions []string
	effortOptions     []string
}

// HandlerConfig carries the scalar runtime config the Handler reads. It is
// populated from the config file's claude + state_dir sections by
// cmd/claude-back/main.go. The shared scalars live in the embedded
// bridgebase.CoreConfig; only claude-specific extras (option lists for the
// interactive pickers) sit at this layer.
//
// PromptTimeout defaults to 0 (disabled): the CLI exits on its own when the
// turn is done, and users abort via /session-abort.
type HandlerConfig struct {
	bridgebase.CoreConfig

	// ModelOptions/PermissionOptions/EffortOptions feed the interactive
	// pickers. Empty falls back to built-in defaults at the call site.
	ModelOptions      []string
	PermissionOptions []string
	EffortOptions     []string
}

// NewWithLogger builds a Handler. rpc is the backend IPC client used to
// emit Control messages; logger is the main component logger.
func NewWithLogger(r *router.Router, api claudeAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	return &Handler{
		Core:              bridgebase.NewCore(r, rpc, cfg.CoreConfig, logger),
		agent:             api,
		modelOptions:      cfg.ModelOptions,
		permissionOptions: cfg.PermissionOptions,
		effortOptions:     cfg.EffortOptions,
	}
}
