package opencodebridge

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
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
// cmd/opencode-back/main.go. PromptTimeout defaults to 0 (disabled): the CLI
// exits on its own when the turn is done, and users abort via /session-abort.
type HandlerConfig struct {
	// DefaultDirectory is reserved as the base for per-chat working dirs but
	// is currently unused: opencode takes its working dir from the /cd pin or
	// an event override (see ensureBinding), never auto-derived. Retained for
	// config parity with the other bridges.
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

// NewWithLogger builds a Handler. rpc is the backend IPC client used to
// emit Control messages; logger is the main component logger.
func NewWithLogger(r *router.Router, api opencodeAPI, rpc *backendrpc.Client, cfg HandlerConfig, logger *log.Logger) *Handler {
	return &Handler{
		Core: bridgebase.NewCore(r, rpc, bridgebase.CoreConfig{
			DefaultDirectory: cfg.DefaultDirectory,
			StateDir:         cfg.StateDir,
			StreamHistory:    cfg.StreamHistory,
			PromptTimeout:    cfg.PromptTimeout,
			DebugRedact:      cfg.DebugRedact,
			WorkspaceRoot:    cfg.WorkspaceRoot,
		}, logger),
		agent: api,
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

// emitPromptNotice emits a Notice bound to promptID (the command's triggering
// message). The frontend terminates that message's progress card in place, so
// a picker failure does not leave the "处理中" placeholder hanging next to a
// standalone error card. Fire-and-forget on a fresh ctx: picker goroutines
// outlive the dispatcher's command ctx.
func (h *Handler) emitPromptNotice(chatID, promptID, level, title, body string) {
	ctx, cancel := context.WithTimeout(h.AppCtx, 10*time.Second)
	defer cancel()
	h.emitLogged(ctx, promptID, chatID, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: body},
	})
}

func (h *Handler) emitAsync(promptID string, ctrl *protocol.Control) {
	h.EmitAsync(promptID, ctrl)
}
