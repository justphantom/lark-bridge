// Package deploymonitor handles /deploy prompts received by lark-deploy-monitor.
//
// The monitor registers as a backend (backendType "deploy-monitor") and runs
// `make <target>` in the configured project root when a bound chat sends
// "/deploy". One deployment runs at a time (single-flight): concurrent
// /deploy prompts get an immediate "in progress" notice instead of queuing.
//
// The result (success or failure, with the deploy script's tail output) is
// sent back as a Notice Control to the originating chatID.
package deploymonitor

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// controlSender is the subset of *backendrpc.Client the handler needs. It
// exists so tests can substitute a fake that captures Controls instead of
// POSTing them.
type controlSender interface {
	SendControl(ctx context.Context, ctrl *protocol.Control) error
}

// statusQuerier is the subset of *backendrpc.Client the handler needs to read
// the frontend's in-flight turn list for /running. Exists so tests substitute
// a fake instead of hitting a real frontend.
type statusQuerier interface {
	Status(ctx context.Context) (*protocol.StatusSnapshot, error)
}

// Commander runs the deploy target in dir. The production implementation
// (cmd/deploy-monitor's execCommander) shells out via os/exec; tests inject
// a fake to avoid a real `make` call. Exported because the production impl
// lives in the main package.
type Commander interface {
	Run(ctx context.Context, dir, target string, args ...string) ([]byte, error)
}

// Config carries the deploy-monitor runtime settings.
type Config struct {
	// ProjectRoot is the repo root where `make` runs. Empty → process CWD.
	ProjectRoot string
	// DeployTarget is the make target (default "deploy", applied in main).
	DeployTarget string
}

// Handler owns the single-flight deploy state and emits Notices back to the
// frontend via the backendrpc client.
type Handler struct {
	cfg     Config
	rpc     controlSender
	status  statusQuerier
	cmd     Commander
	logger  *log.Logger
	timeout time.Duration

	mu      sync.Mutex
	running bool
}

// New wires the handler. status supplies the in-flight turn snapshot for
// /running (typically the same *backendrpc.Client as rpc). deployTimeout
// bounds one `make` run; <=0 → 10m.
func New(cfg Config, rpc controlSender, status statusQuerier, cmd Commander, logger *log.Logger, deployTimeout time.Duration) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	if deployTimeout <= 0 {
		deployTimeout = 10 * time.Minute
	}
	return &Handler{cfg: cfg, rpc: rpc, status: status, cmd: cmd, logger: logger, timeout: deployTimeout}
}

// HandleEvent dispatches Prompt events. Only "/deploy" is honored; any other
// text is rejected with a notice. The actual deploy runs asynchronously so
// the SSE event loop is not blocked.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		return nil
	}
	chatID := ev.Prompt.ChatID
	prompt := strings.TrimSpace(ev.Prompt.Text)

	var force bool
	switch prompt {
	case "/deploy":
	case "/deploy-force":
		force = true
	case "/running":
		// Read-only query; must NOT take the single-flight deploy slot — a
		// /running while a deploy is in progress should still answer.
		return h.handleRunning(ctx, chatID)
	default:
		return h.notify(ctx, chatID, "warning", "未知指令",
			"本后端接受 /deploy、/deploy-force（在 "+h.cfg.ProjectRoot+" 执行 make "+h.cfg.DeployTarget+"）或 /running（查看运行中会话）。")
	}

	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		h.logger.Info("deploy rejected: already running", log.FieldChatID, chatID)
		return h.notify(ctx, chatID, "warning", "部署进行中",
			"已有一次部署正在执行，请等待其完成后再试。")
	}
	h.running = true
	h.mu.Unlock()

	go h.runDeploy(chatID, force) //nolint:gosec // G118: deploy must outlive the triggering request's ctx
	label := "make " + h.cfg.DeployTarget
	if force {
		label += " ARGS=--force"
	}
	return h.notify(ctx, chatID, "info", "部署已触发",
		"开始执行 "+label+"，完成后会在此通知。")
}

// runDeploy executes the make target and emits the terminal notice. It runs
// on its own goroutine so the SSE loop stays free. The single-flight flag is
// always cleared on exit (including ctx cancel).
func (h *Handler) runDeploy(chatID string, force bool) {
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	h.logger.Info("deploy start",
		log.FieldChatID, chatID,
		"dir", h.cfg.ProjectRoot,
		"target", h.cfg.DeployTarget,
		"force", force)

	var makeArgs []string
	if force {
		makeArgs = []string{"ARGS=--force"}
	}
	out, err := h.cmd.Run(ctx, h.cfg.ProjectRoot, h.cfg.DeployTarget, makeArgs...)
	if err != nil {
		h.logger.Error("deploy failed", log.FieldChatID, chatID, log.FieldError, err)
		h.notifyWithRetry(chatID, "error", "部署失败",
			tailOutput(out, 500)+"\n错误："+err.Error())
		return
	}

	h.logger.Info("deploy done", log.FieldChatID, chatID)
	h.notifyWithRetry(chatID, "success", "部署完成", tailOutput(out, 500))
}

// notifyWithRetry sends a notice to the chat, retrying when the frontend
// returns 503 "backend not registered" — which happens after a redeploy:
// feishu-front restarts, and deploy-monitor's SSE reconnect lands a few
// seconds later. Until the SSE is re-established, POST /v1/control returns
// 503 because the backend is not in the frontend's registry. We poll the
// reconnect with 2s intervals up to 30s total.
func (h *Handler) notifyWithRetry(chatID, level, title, message string) {
	for attempt := range 15 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := h.notify(ctx, chatID, level, title, message)
		cancel()
		if err == nil {
			return
		}
		h.logger.Warn("deploy notify attempt failed, retrying",
			log.FieldChatID, chatID, "attempt", attempt+1, log.FieldError, err)
		time.Sleep(2 * time.Second)
	}
	h.logger.Error("deploy notify gave up after retries", log.FieldChatID, chatID)
}

// notify emits a Notice Control to chatID. ChatID is required by the frontend
// validator for TypeNotice, so an empty chatID is rejected up front rather
// than letting SendControl's Validate fail with an opaque message.
func (h *Handler) notify(ctx context.Context, chatID, level, title, message string) error {
	if chatID == "" {
		return fmt.Errorf("notify: chatID is empty")
	}
	return h.rpc.SendControl(ctx, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: message},
	})
}

// handleRunning answers the /running query: fetches the frontend's in-flight
// turn snapshot and renders it as a notice. It runs inline (not on a goroutine)
// — the GET is bounded by statusQueryTimeout and is user-paced, so blocking
// the SSE loop briefly is acceptable, unlike a multi-minute `make deploy`.
func (h *Handler) handleRunning(ctx context.Context, chatID string) error {
	snap, err := h.status.Status(ctx)
	if err != nil {
		return h.notify(ctx, chatID, "error", "查询失败", "读取运行中会话失败："+err.Error())
	}
	return h.notify(ctx, chatID, "info", "运行中会话", renderTurns(snap))
}

// renderTurns formats the in-flight snapshot as a scannable notice body. The
// trailing abort hint reinforces the policy: turns are never ended automatically.
func renderTurns(snap *protocol.StatusSnapshot) string {
	if len(snap.Turns) == 0 {
		return "当前没有运行中的会话。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "运行中会话（%d）：\n", len(snap.Turns))
	for _, t := range snap.Turns {
		fmt.Fprintf(&sb, "- %s · %s · %s\n", t.BackendID, shortID(t.ChatID), formatElapsed(t.ElapsedS))
	}
	sb.WriteString("\n会话不会自动结束，如需结束请发送 /session-abort。")
	return sb.String()
}

// shortID shortens a Feishu ID (oc_ + 32 hex) to its last 8 chars so the turn
// list stays scannable while remaining identifiable.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-8:]
}

// formatElapsed turns seconds into a compact duration label.
func formatElapsed(s int64) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	}
}

// tailOutput returns the last maxBytes of out as a string. The deploy script
// emits substantial progress text; only the tail is useful in a chat notice.
func tailOutput(out []byte, maxBytes int) string {
	s := strings.TrimSpace(string(out))
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	return "…" + s[len(s)-maxBytes:]
}
