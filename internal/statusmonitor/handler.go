// Package statusmonitor drives the lark-status-monitor backend.
//
// It registers as a backend (backendType "status-monitor") and, on a fixed
// interval, queries the frontend's GET /v1/status snapshot and emits a
// TypeStatusReport Control. The frontend broadcasts a standing overview card
// to every chat bound to this backend, PATCHing it in place each tick (and
// re-sending if a user deleted it). It is push-only: a Prompt sent to a bound
// chat gets an explanatory Notice, not business logic.
package statusmonitor

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// controlSender / statusQuerier are lifted to internal/backendrpc (shared
// with deploymonitor and miniagent). Local type aliases keep the call sites
// readable.
type controlSender = backendrpc.ControlSender
type statusQuerier = backendrpc.StatusQuerier

// Config carries the status-monitor runtime settings.
type Config struct {
	// Interval is the refresh period. <=0 → New defaults to 60s.
	Interval time.Duration
	// Title is the card title. Empty → "lark-bridge 状态总览".
	Title string
}

// defaultTitle is applied in New when Config.Title is empty.
const defaultTitle = "lark-bridge 状态总览"

// Handler owns the refresh ticker. Push-only: it never answers prompts with
// business logic (HandleEvent only replies with an explanatory Notice).
type Handler struct {
	cfg       Config
	rpc       controlSender
	status    statusQuerier
	backendID string
	now       func() time.Time // injectable clock for deterministic tests
	logger    *log.Logger
}

// New wires the handler. rpc and status are typically the same
// *backendrpc.Client. A non-positive Interval defaults to 60s.
func New(cfg Config, rpc controlSender, status statusQuerier, backendID string, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 60 * time.Second
	}
	if cfg.Title == "" {
		cfg.Title = defaultTitle
	}
	return &Handler{cfg: cfg, rpc: rpc, status: status, backendID: backendID, now: time.Now, logger: logger}
}

// Run emits one report immediately, then every Interval until ctx is cancelled.
// Call on its own goroutine so backendrpc.Run's SSE event loop is never
// blocked; both share ctx so SIGTERM cancels them together.
func (h *Handler) Run(ctx context.Context) error {
	h.tick(ctx)
	t := time.NewTicker(h.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			h.tick(ctx)
		}
	}
}

// buildStatusReport fetches one /v1/status snapshot and assembles the
// standing-card StatusReport Control (without sending). Shared by the tick
// loop and the /status command so the on-demand refresh shows exactly the
// same frame the periodic push would. A snapshot-fetch failure yields the
// error verbatim; the caller decides whether to skip silently (tick) or
// surface it (command).
func (h *Handler) buildStatusReport(ctx context.Context) (*protocol.Control, error) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := h.status.Status(qctx)
	if err != nil {
		return nil, err
	}
	return &protocol.Control{
		Type: protocol.TypeStatusReport,
		StatusReport: &protocol.StatusReportPayload{
			Key:         h.backendID,
			GeneratedAt: h.now().Unix(),
			IntervalS:   int(h.cfg.Interval.Seconds()),
			Title:       h.cfg.Title,
			InFlight:    snap.InFlight,
			Backends:    snap.Backends,
			Turns:       snap.Turns,
			Hosts:       snap.Hosts,
			Services:    snap.Services,
		},
	}, nil
}

// tick assembles one StatusReport from the current /v1/status snapshot and
// POSTs it. A failed snapshot fetch skips the tick (no error card) so the
// standing card keeps showing the last good frame; a failed POST logs and
// moves on — the next tick retries, and the frontend's per-chat PATCH is
// idempotent, so a gap never stacks a duplicate card.
func (h *Handler) tick(ctx context.Context) {
	ctrl, err := h.buildStatusReport(ctx)
	if err != nil {
		h.logger.Warn("status query failed", log.FieldError, err)
		return
	}
	if err := h.rpc.SendControl(ctx, ctrl); err != nil {
		h.logger.Warn("status report send failed", log.FieldError, err)
	}
}

// HandleEvent dispatches Prompt events addressed to the status-monitor backend.
// Unlike the push-only tick, this is the interactive command surface so a user
// bound to the status backend can probe on demand:
//   - /status, /refresh → push one StatusReport now (refreshes the standing
//     card) and finalise the command's own card.
//   - /running  → list in-flight turns (same shape as deploy-monitor's).
//   - /backends → list online backends.
//   - /help, "", or any unknown text → enumerate the commands.
//
// Answer / Abort are ignored (no interaction model to drive); Ping is
// answered with a TypePong heartbeat. Every
// branch finalises the triggering promptID so the command's progress card does
// not orphan into /v1/status InFlight. Returns nil on a non-Prompt event so
// backendrpc.Run's loop never aborts on one.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	// TypePing: the frontend's C2 app-level health probe. Answer on this
	// dispatch loop itself — a wedged loop never pongs and the frontend
	// evicts the backend after maxMissedPongs. Fire-and-forget with its own
	// short ctx: pong is disposable and must not stall the loop on slow IPC.
	// PromptID stays empty (pong is keyed by the URL-path BackendID).
	if ev.Type == protocol.TypePing {
		bridgebase.GoSafe(h.logger, "pong", func() {
			pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := h.rpc.SendControl(pctx, &protocol.Control{Type: protocol.TypePong, Pong: &protocol.PongPayload{}}); err != nil {
				h.logger.Debug("status-monitor: pong reply failed", log.FieldError, err)
			}
		})
		return nil
	}
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		return nil
	}
	chatID := ev.Prompt.ChatID
	if chatID == "" {
		return nil
	}
	promptID := ev.PromptID
	cardMsgID := ev.Prompt.CardMessageID
	switch strings.TrimSpace(ev.Prompt.Text) {
	case "/status", "/refresh":
		return h.handleStatusRefresh(ctx, chatID, promptID, cardMsgID)
	case "/running":
		return h.handleRunningQuery(ctx, chatID, promptID, cardMsgID)
	case "/backends":
		return h.handleBackendsQuery(ctx, chatID, promptID, cardMsgID)
	default:
		return h.notify(ctx, chatID, promptID, cardMsgID, "info", "状态总览", statusHelpBody(h.cfg.Interval))
	}
}

// handleStatusRefresh pushes one StatusReport immediately (refreshing the
// standing overview card without waiting for the next tick) then finalises the
// command's own progress card with a short notice. The StatusReport is a
// non-terminal broadcast keyed by backendID, so the explicit notice is what
// closes the turn and keeps /v1/status InFlight honest.
func (h *Handler) handleStatusRefresh(ctx context.Context, chatID, promptID, cardMsgID string) error {
	ctrl, err := h.buildStatusReport(ctx)
	if err != nil {
		return h.notify(ctx, chatID, promptID, cardMsgID, "error", "刷新失败", "读取状态失败："+err.Error())
	}
	if err := h.rpc.SendControl(ctx, ctrl); err != nil {
		h.logger.Warn("status report send failed", log.FieldError, err)
	}
	return h.notify(ctx, chatID, promptID, cardMsgID, "success", "已刷新", "总览卡已刷新为最新状态。")
}

// handleRunningQuery renders the in-flight turn snapshot as a notice bound to
// promptID. Mirrors deploy-monitor's /running so users see the same shape from
// either backend.
func (h *Handler) handleRunningQuery(ctx context.Context, chatID, promptID, cardMsgID string) error {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := h.status.Status(qctx)
	if err != nil {
		return h.notify(ctx, chatID, promptID, cardMsgID, "error", "查询失败", "读取运行中会话失败："+err.Error())
	}
	return h.notify(ctx, chatID, promptID, cardMsgID, "info", "运行中会话", renderRunningTurns(snap))
}

// handleBackendsQuery lists the online backends reported by the snapshot.
func (h *Handler) handleBackendsQuery(ctx context.Context, chatID, promptID, cardMsgID string) error {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := h.status.Status(qctx)
	if err != nil {
		return h.notify(ctx, chatID, promptID, cardMsgID, "error", "查询失败", "读取后端列表失败："+err.Error())
	}
	return h.notify(ctx, chatID, promptID, cardMsgID, "info", "在线后端", renderBackends(snap))
}

// notify sends one terminal notice bound to promptID (finalising the command's
// progress card) and patching cardMsgID by raw message_id when present.
func (h *Handler) notify(ctx context.Context, chatID, promptID, cardMsgID, level, title, message string) error {
	return h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeNotice,
		PromptID: promptID,
		ChatID:   chatID,
		Notice: &protocol.NoticePayload{
			Level:           level,
			Title:           title,
			Message:         message,
			UpdateMessageID: cardMsgID,
		},
	})
}

// statusHelpBody enumerates the status-monitor commands. Serves /help, the
// empty-prompt fallback, and any unknown text.
func statusHelpBody(interval time.Duration) string {
	return fmt.Sprintf("本后端为状态总览，每 %s 自动刷新。可用指令：\n"+
		"- /status 或 /refresh：立即刷新总览卡\n"+
		"- /running：查看运行中的会话\n"+
		"- /backends：列出在线后端\n"+
		"- /help：显示本帮助", interval)
}

// renderRunningTurns formats the in-flight snapshot as a notice body.
func renderRunningTurns(snap *protocol.StatusSnapshot) string {
	if len(snap.Turns) == 0 {
		return "当前没有运行中的会话。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "运行中会话（%d）：\n", len(snap.Turns))
	for _, t := range snap.Turns {
		fmt.Fprintf(&sb, "- %s · %s · %s\n", t.BackendID, shortStatusID(t.ChatID), formatStatusElapsed(t.ElapsedS))
	}
	return sb.String()
}

// renderBackends lists the online backends from the snapshot.
func renderBackends(snap *protocol.StatusSnapshot) string {
	if len(snap.Backends) == 0 {
		return "当前没有在线后端。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "在线后端（%d）：\n", len(snap.Backends))
	for _, b := range snap.Backends {
		fmt.Fprintf(&sb, "- %s\n", b)
	}
	return sb.String()
}

// shortStatusID shortens a Feishu ID to its last 8 chars so the turn list
// stays scannable while remaining identifiable.
func shortStatusID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return "…" + id[len(id)-8:]
}

// formatStatusElapsed turns seconds into a compact duration label.
func formatStatusElapsed(s int64) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60)
	}
}
