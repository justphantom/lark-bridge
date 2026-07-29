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
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
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

// tick assembles one StatusReport from the current /v1/status snapshot and
// POSTs it. A failed snapshot fetch skips the tick (no error card) so the
// standing card keeps showing the last good frame; a failed POST logs and
// moves on — the next tick retries, and the frontend's per-chat PATCH is
// idempotent, so a gap never stacks a duplicate card.
func (h *Handler) tick(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snap, err := h.status.Status(qctx)
	if err != nil {
		h.logger.Warn("status query failed", log.FieldError, err)
		return
	}
	ctrl := &protocol.Control{
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
	}
	if err := h.rpc.SendControl(ctx, ctrl); err != nil {
		h.logger.Warn("status report send failed", log.FieldError, err)
	}
}

// HandleEvent replies to any Prompt with an explanatory Notice so a user who
// messages the bound chat learns the card refreshes on its own. Answer /
// Abort / Ping are ignored — there is no interaction model to drive. Returns
// nil on a non-Prompt event so backendrpc.Run's loop never aborts on one.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		return nil
	}
	chatID := ev.Prompt.ChatID
	if chatID == "" {
		return nil
	}
	return h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeNotice,
		PromptID: ev.PromptID,
		ChatID:   chatID,
		Notice: &protocol.NoticePayload{
			Level:   "info",
			Title:   "状态总览",
			Message: "此后端为状态总览，无需发送指令；总览卡每 " + h.cfg.Interval.String() + " 自动刷新。",
		},
	})
}
