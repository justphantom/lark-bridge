package ompbridge

import (
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
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
	// maxAutoRetries caps how many consecutive auto_retry_start events are
	// tolerated before the bridge aborts the turn. <=0 means unlimited.
	maxAutoRetries int
	// gcColdArchiveAfterDays / gcRetainNewestPerCwd / gcTimeout feed
	// /session-gc's invocation of `omp gc --apply --archive`.
	gcColdArchiveAfterDays int
	gcRetainNewestPerCwd   int
	gcTimeout              time.Duration
}

// HandlerConfig carries the scalar runtime config the Handler reads. It is
// populated from the config file's omp + state_dir sections by
// cmd/omp-back/main.go. The shared scalars live in the embedded
// bridgebase.CoreConfig; only omp-specific extras sit at this layer.
// ApprovalDefault maps onto CoreConfig.PermissionDefault (semantic equivalent
// of claude's permission_mode) so /current and the Core's display path work
// without ompbridge-specific plumbing.
//
// PromptTimeout defaults to 0 (disabled): the CLI exits on its own when the
// turn is done, and users abort via /session-abort.
type HandlerConfig struct {
	bridgebase.CoreConfig

	// ThinkingDefault is the process-level --thinking fallback. Used only
	// when a chat's effort pin is empty. Kept at this layer (NOT on
	// CoreConfig) to avoid name collisions with claude's effort concept.
	ThinkingDefault string

	// ModelOptions/ApprovalOptions/ThinkingOptions feed the interactive
	// pickers. Empty falls back to built-in defaults at the call site.
	ModelOptions    []string
	ApprovalOptions []string
	ThinkingOptions []string

	// MaxAutoRetries caps how many consecutive auto_retry_start events are
	// tolerated before the bridge aborts the turn. <=0 means unlimited
	// (default: 3 from config defaults).
	MaxAutoRetries int

	// GCColdArchiveAfterDays / GCRetainNewestPerCwd / GCTimeout feed
	// /session-gc's invocation of `omp gc --apply --archive`. Zero values
	// are filled by config applyDefaults.
	GCColdArchiveAfterDays int
	GCRetainNewestPerCwd   int
	GCTimeout              time.Duration
}

// NewWithLogger builds a Handler. rpc is the backend IPC client used to emit
// Control messages; logger is the main component logger.
func NewWithLogger(r *router.Router, api ompAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	return &Handler{
		Core:                   bridgebase.NewCore(r, rpc, cfg.CoreConfig, logger),
		agent:                  api,
		thinkingDefault:        cfg.ThinkingDefault,
		modelOptions:           cfg.ModelOptions,
		approvalOptions:        cfg.ApprovalOptions,
		thinkingOptions:        cfg.ThinkingOptions,
		maxAutoRetries:         cfg.MaxAutoRetries,
		gcColdArchiveAfterDays: cfg.GCColdArchiveAfterDays,
		gcRetainNewestPerCwd:   cfg.GCRetainNewestPerCwd,
		gcTimeout:              cfg.GCTimeout,
	}
}
