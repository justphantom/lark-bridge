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

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// confirmTimeout bounds how long a /deploy-some / /deploy-force picker waits
// for the user's submission. It MUST be >= cardkit.InteractiveTimeout: the
// picker card advertises that lifetime to the user ("⏳ 等待你的确认（10 分钟后
// 自动失效）"), and if the backend gave up first a submission in the gap would
// be silently dropped — the AnswerBroker slot would be gone, so Deliver
// returns false and the deploy never runs while the card still looks alive.
// Derived from the card's lifetime plus a buffer that absorbs the IPC
// round-trip + clock skew, so the card expires before the wait does (a click
// on an expired card is rejected by the frontend, not silently swallowed here).
const confirmTimeout = cardkit.InteractiveTimeout + time.Minute

// deployNoticeTimeout bounds a single deploy notice POST (the progress banner,
// the busy-reject notice, each notifyWithRetry attempt). A picker's own wait
// deadline is deliberately NOT reused for this: a submission near the
// picker-timeout boundary would otherwise leave almost no budget for the
// banner POST, and a failed banner used to strand h.running=true forever.
const deployNoticeTimeout = 10 * time.Second

// controlSender / statusQuerier are lifted to internal/backendrpc (interfaces
// shared with miniagent and statusmonitor). The local names are kept as
// type aliases so existing call sites stay readable.
type controlSender = backendrpc.ControlSender
type statusQuerier = backendrpc.StatusQuerier

// Commander runs a command (name with args) inside dir. The production
// implementation (cmd/deploy-monitor's execCommander) shells out via os/exec;
// tests inject a fake to avoid a real subprocess. Exported because the
// production impl lives in the main package.
type Commander interface {
	Run(ctx context.Context, dir, name string, args ...string) ([]byte, error)
}

// Config carries the deploy-monitor runtime settings.
type Config struct {
	// ProjectRoot is the repo root where `make` runs. Empty → process CWD.
	ProjectRoot string
	// DeployTarget is the make target (default "deploy", applied in main).
	DeployTarget string
}

// Handler owns the single-flight deploy state and emits Notices back to the
// frontend via the backendrpc client. answers routes /deploy-force confirm
// card clicks back to the goroutine waiting on the confirmation.
type Handler struct {
	cfg     Config
	rpc     controlSender
	status  statusQuerier
	cmd     Commander
	logger  *log.Logger
	timeout time.Duration
	answers *bridgebase.AnswerBroker

	mu      sync.Mutex
	running bool

	// jobWg tracks in-flight runJob goroutines so Close can wait for a deploy
	// /pull /push to finish instead of being SIGKILLed mid-run (which would
	// leave docker half-built or git half-pushed). The job's own ctx timeout
	// still bounds its length; Close just refuses to abandon it early.
	jobWg sync.WaitGroup
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
	return &Handler{cfg: cfg, rpc: rpc, status: status, cmd: cmd, logger: logger, timeout: deployTimeout, answers: bridgebase.NewAnswerBroker()}
}

// HandleEvent dispatches Prompt events. /deploy, /deploy-force, /pull and /push
// share one single-flight slot (acquireAndRun) so a deploy mid-flight can't
// race a pull/push that would mutate the working tree; /running is read-only
// and bypasses the slot. The job runs asynchronously so the SSE event loop is
// not blocked. Every notice binds the triggering promptID so the frontend
// patches the command's progress card in place and finalises the turn,
// instead of orphaning a "处理中" card (an orphaned turn inflates
// /v1/status InFlight, which can block deploy.sh).
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	// A /deploy-force confirm card click arrives as a TypeAnswer; route it to
	// the goroutine blocked in awaitForceConfirm. Unknown/late answers (the
	// goroutine already timed out) are dropped by Deliver.
	if ev.Type == protocol.TypeAnswer && ev.Answer != nil {
		h.answers.Deliver(ev.Answer.RequestID, ev.Answer)
		return nil
	}
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
				h.logger.Debug("deploy-monitor: pong reply failed", log.FieldError, err)
			}
		})
		return nil
	}
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		return nil
	}
	chatID := ev.Prompt.ChatID
	promptID := ev.PromptID
	prompt := strings.TrimSpace(ev.Prompt.Text)
	// cardMsgID is the frontend's progress-card message_id for this prompt.
	// Echoing it back as Notice.UpdateMessageID lets the terminal notice
	// patch that card even after `make deploy` restarts the frontend and
	// wipes its turn map (the message_id itself is Feishu-side and survives).
	cardMsgID := ev.Prompt.CardMessageID

	switch prompt {
	case "/deploy":
		// /deploy now requires explicit confirmation before starting the full
		// deploy, matching /deploy-force's confirmation gate.
		return h.confirmAndDeploy(ctx, chatID, promptID, cardMsgID)
	case "/deploy-some":
		// /deploy-some pops a multi-select card; the deploy runs only after
		// the user submits a non-empty subset, which becomes --services=<csv>.
		return h.confirmAndDeploySome(ctx, chatID, promptID, cardMsgID)
	case "/deploy-force":
		// /deploy-force passes ARGS=--force to deploy.sh, skipping safety
		// checks — a one-click destructive deploy is too easy to fire by
		// mistake, so gate it behind a confirm card.
		return h.confirmAndDeployForce(ctx, chatID, promptID, cardMsgID)
	case "/pull":
		// --ff-only: refuse to create a merge on divergence instead of
		// leaving a conflicted working tree that would block later deploys.
		return h.acquireAndRun(chatID, promptID, cardMsgID, "git", []string{"pull", "--ff-only"}, "拉取")
	case "/push":
		return h.acquireAndRun(chatID, promptID, cardMsgID, "git", []string{"push"}, "推送")
	case "/running":
		// Read-only query; must NOT take the single-flight slot — a /running
		// while a job is in progress should still answer.
		return h.handleRunning(ctx, chatID, promptID, cardMsgID)
	default:
		return h.notify(ctx, chatID, promptID, cardMsgID, "warning", "未知指令",
			"本后端接受 /deploy（全量）、/deploy-some（多选服务子集）、/deploy-force（需确认）、/pull（git pull --ff-only）、/push（git push）或 /running（查看运行中会话）。")
	}
}

// deployArgs assembles the make argument list for the deploy target. services
// (non-empty) appends ARGS=--services=<csv>; force appends --force inside the
// same ARGS override. deploy.sh parses both via parse_args. The csv is what
// /deploy-some's multi-select submits; /deploy passes nil for a full deploy.
func (h *Handler) deployArgs(force bool, services []string) []string {
	args := []string{h.cfg.DeployTarget}
	var overrides []string
	if len(services) > 0 {
		overrides = append(overrides, "--services="+strings.Join(services, ","))
	}
	if force {
		overrides = append(overrides, "--force")
	}
	if len(overrides) > 0 {
		args = append(args, "ARGS="+strings.Join(overrides, " "))
	}
	return args
}

// /deploy-force's confirmation gate (confirmAndDeployForce / awaitForceConfirm)
// lives in confirm.go.

// acquireAndRun takes the single-flight slot and launches the job on its own
// goroutine. On accept it emits a non-terminal progress banner bound to
// promptID (so the job surfaces on the command's own progress card, no
// separate "triggered" card); on busy it emits a terminal warning notice
// bound to promptID (finalising the rejected command's card). The terminal
// success/error notice fires from the goroutine, likewise bound — and
// carrying cardMsgID as UpdateMessageID so it patches the progress card by
// raw message_id even if the job restarted the frontend in between.
//
// The notice POSTs use a self-derived short ctx (deployNoticeTimeout), NOT
// the caller's ctx. /deploy-some / /deploy-force reach this from a picker-wait
// ctx whose deadline shrinks as the user takes longer to submit; reusing it
// would starve the banner POST near the timeout boundary. busy is NOT
// returned as an error: a failed busy-notice was previously mislabelled by
// callers (awaitDeploySome/awaitForceConfirm) as "部署失败". It is now logged
// best-effort and the rejection returns nil.
func (h *Handler) acquireAndRun(chatID, promptID, cardMsgID, name string, args []string, label string) error {
	h.mu.Lock()
	if h.running {
		h.mu.Unlock()
		h.logger.Info("job rejected: already running",
			log.FieldChatID, chatID, "cmd", jobLabel(name, args))
		nctx, cancel := context.WithTimeout(context.Background(), deployNoticeTimeout)
		defer cancel()
		if err := h.notify(nctx, chatID, promptID, cardMsgID, "warning", label+"进行中",
			"已有一次操作正在执行，请等待其完成后再试。"); err != nil {
			h.logger.Warn("busy-reject notice failed",
				log.FieldChatID, chatID, log.FieldError, err)
		}
		return nil
	}
	h.running = true
	h.mu.Unlock()

	progCtx, cancel := context.WithTimeout(context.Background(), deployNoticeTimeout)
	defer cancel()
	if err := h.notifyProgress(progCtx, chatID, promptID, "⏳ "+label+"执行中…"); err != nil {
		// Roll back the slot: runJob never started, so the defer inside runJob
		// that clears h.running will never run. Without this rollback a single
		// failed banner POST would wedge single-flight forever (every later
		// /deploy / /pull / /push rejected as "进行中").
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
		return err
	}
	// Track the job under jobWg BEFORE the goroutine starts so Close (which
	// may run the instant backendrpc.Run returns on SIGTERM) cannot race ahead
	// of the Add and observe an empty WaitGroup. The job runs on its own ctx
	// (runJob builds a context.Background()-rooted timeout) precisely so it
	// outlives the triggering request — and the process, until Close returns.
	h.jobWg.Add(1)
	// GoSafe so a panic inside runJob (git/docker output parsing, a bad cmd)
	// is recovered + logged instead of crashing the backend. jobWg.Done is
	// deferred inside fn so it still runs during the panic unwind (Close does
	// not hang waiting on a dead job) before GoSafe's recover catches it. The
	// job still runs on its own context.Background()-rooted timeout inside
	// runJob so it outlives the triggering request's ctx.
	bridgebase.GoSafe(h.logger, "deploy-monitor job: "+label, func() {
		defer h.jobWg.Done()
		h.runJob(chatID, promptID, cardMsgID, name, args, label)
	})
	return nil
}

// Close waits up to ctx for any in-flight job to finish. A deploy/pull/push
// is never cancelled mid-run (cutting it short leaves docker/git in a broken
// state); the job's own timeout still bounds it. main calls this after
// backendrpc.Run returns so SIGTERM does not SIGKILL a running make.
func (h *Handler) Close(ctx context.Context) error {
	// Unblock picker waits first: awaitDeploySome/awaitForceConfirm goroutines
	// are not in jobWg and would otherwise sit on their answer channel until
	// process exit, leaving the user's card forever at "待选择". Drain closes
	// every slot so each blocked picker emits its 已取消 notice and returns.
	h.answers.Drain()
	done := make(chan struct{})
	go func() { h.jobWg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// runJob runs name args in ProjectRoot and emits the terminal notice. It runs
// on its own goroutine so the SSE loop stays free. The single-flight flag is
// always cleared on exit (including ctx cancel). The terminal notice is bound
// to promptID AND carries cardMsgID as UpdateMessageID: after a /deploy that
// restarts the frontend the turn is gone, but the message_id is Feishu-side
// and survives, so the frontend patches the original progress card via the
// UpdateMessageID fast path instead of stacking a standalone card.
func (h *Handler) runJob(chatID, promptID, cardMsgID, name string, args []string, label string) {
	defer func() {
		h.mu.Lock()
		h.running = false
		h.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), h.timeout)
	defer cancel()

	h.logger.Info("job start",
		log.FieldChatID, chatID,
		"dir", h.cfg.ProjectRoot,
		"cmd", jobLabel(name, args))

	out, err := h.cmd.Run(ctx, h.cfg.ProjectRoot, name, args...)

	// Release the single-flight slot BEFORE the terminal-notice retry loop.
	// notifyWithRetry polls a frontend that may be mid-redeploy for up to ~3
	// min (15 × retry budget); holding the slot for that whole window would
	// reject every /deploy /pull /push in every chat (Low#4). The notice is
	// idempotent, so a new job starting concurrently is harmless. The deferred
	// clear above stays as a panic safety net (idempotent on the happy path).
	h.mu.Lock()
	h.running = false
	h.mu.Unlock()

	if err != nil {
		h.logger.Error("job failed", log.FieldChatID, chatID, "cmd", jobLabel(name, args), log.FieldError, err)
		h.notifyWithRetry(chatID, promptID, cardMsgID, "error", label+"失败",
			tailOutput(out, 500)+"\n错误："+err.Error())
		return
	}

	h.logger.Info("job done", log.FieldChatID, chatID, "cmd", jobLabel(name, args))
	h.notifyWithRetry(chatID, promptID, cardMsgID, "success", label+"完成", tailOutput(out, 500))
}

// jobLabel lives in render.go alongside the other formatting helpers.

// notifyWithRetry sends a notice to the chat, retrying when the frontend
// returns 503 "backend not registered" — which happens after a redeploy:
// feishu-front restarts, and deploy-monitor's SSE reconnect lands a few
// seconds later. Until the SSE is re-established, POST /v1/control returns
// 503 because the backend is not in the frontend's registry. We poll the
// reconnect with 2s intervals up to 30s total. promptID + cardMsgID bind the
// notice to the command's card (see runJob).
func (h *Handler) notifyWithRetry(chatID, promptID, cardMsgID, level, title, message string) {
	for attempt := range 15 {
		ctx, cancel := context.WithTimeout(context.Background(), deployNoticeTimeout)
		err := h.notify(ctx, chatID, promptID, cardMsgID, level, title, message)
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

// notify emits a Notice Control to chatID, bound to promptID and carrying
// cardMsgID as UpdateMessageID (empty tolerated — the frontend then falls
// back to its promptID turn lookup). ChatID is required by the frontend
// validator for TypeNotice, so an empty chatID is rejected up front rather
// than letting SendControl's Validate fail with an opaque message.
func (h *Handler) notify(ctx context.Context, chatID, promptID, cardMsgID, level, title, message string) error {
	if chatID == "" {
		return fmt.Errorf("notify: chatID is empty")
	}
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

// notifyProgress emits a non-terminal progress banner bound to promptID so an
// async job surfaces on the command's own progress card without spawning a
// separate "triggered" card. A later notify (terminal) patches the same card
// and finalises the turn.
func (h *Handler) notifyProgress(ctx context.Context, chatID, promptID, description string) error {
	if chatID == "" {
		return fmt.Errorf("notifyProgress: chatID is empty")
	}
	return h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeProgress,
		PromptID: promptID,
		ChatID:   chatID,
		Progress: &protocol.ProgressPayload{Description: description},
	})
}

// handleRunning answers the /running query: fetches the frontend's in-flight
// turn snapshot and renders it as a notice bound to promptID (so the
// command's card is finalised). It runs inline (not on a goroutine) — the GET
// is bounded by statusQueryTimeout and is user-paced, so blocking the SSE
// loop briefly is acceptable, unlike a multi-minute `make deploy`.
func (h *Handler) handleRunning(ctx context.Context, chatID, promptID, cardMsgID string) error {
	snap, err := h.status.Status(ctx)
	if err != nil {
		return h.notify(ctx, chatID, promptID, cardMsgID, "error", "查询失败", "读取运行中会话失败："+err.Error())
	}
	return h.notify(ctx, chatID, promptID, cardMsgID, "info", "运行中会话", renderTurns(snap))
}
