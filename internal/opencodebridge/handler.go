package opencodebridge

import (
	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// Handler is the opencode-back orchestrator. One per process. The
// backend-agnostic spine (router, IPC client, per-chat cancel tracking,
// answer broker, emit helpers, shutdown) lives in the embedded
// bridgebase.Core; opencode-back adds only its CLI client (model/agent
// options come from the CLI's list subcommands, not config).
type Handler struct {
	*bridgebase.Core

	agent opencodeAPI
}

// HandlerConfig carries the scalar runtime config the Handler reads. It is
// populated from the config file's opencode + state_dir sections by
// cmd/opencode-back/main.go. The shared scalars live in the embedded
// bridgebase.CoreConfig; opencode has no extra backend-specific scalars
// (model/agent options come from the CLI's list subcommands, not config).
//
// PromptTimeout defaults to 0 (disabled): the CLI exits on its own when the
// turn is done, and users abort via /session-abort.
type HandlerConfig struct {
	bridgebase.CoreConfig
}

// NewWithLogger builds a Handler. rpc is the backend IPC client used to
// emit Control messages; logger is the main component logger.
func NewWithLogger(r *router.Router, api opencodeAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	return &Handler{
		Core:  bridgebase.NewCore(r, rpc, cfg.CoreConfig, logger),
		agent: api,
	}
}
