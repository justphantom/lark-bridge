package ompbridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/streamarchive"
	"github.com/justphantom/lark-bridge/internal/usage"
)

// cancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled.
const cancelNoticeTimeout = 5 * time.Second

// runPrompt drives one omp turn for chatID: it starts an `omp` CLI subprocess,
// streams its events, and emits the terminal control. The `session` header's
// session id is back-filled onto the binding so the next turn resumes it.
func (h *Handler) runPrompt(parent context.Context, chatID string, binding router.Binding, prompt, replyToID string, mine *bridgebase.PromptCancel) {
	// Recover so a panic in this goroutine never crashes the process.
	defer func() {
		if r := recover(); r != nil {
			h.Logger.Error("panic in runPrompt",
				log.FieldChatID, chatID,
				log.FieldPanic, r,
				log.FieldStack, debug.Stack())
			// Gate on appCtx, not parent: parent is cancelled by mine.Cancel()
			// below so reading it here would always see "cancelled".
			if h.AppCtx.Err() == nil {
				h.emitLogged(context.Background(), replyToID, chatID, &protocol.Control{
					Type:   protocol.TypeNotice,
					ChatID: chatID,
					Notice: &protocol.NoticePayload{Level: "error", Title: "内部错误", Message: "⚠️ 内部错误，已恢复"},
				})
			}
		}
	}()
	// Mark the prompt done after endPrompt/cancel unwind (LIFO) and before the
	// recover above, so Close's waitPrompts unblocks only when the goroutine
	// has fully released its slot — including the subprocess kill on cancel.
	defer h.Wg.Done()
	defer h.EndPrompt(chatID, mine)
	defer mine.Cancel()

	// Re-read the binding here rather than trusting the snapshot the caller
	// took in handlePromptEvent: a concurrent /cd, /session-del or /model
	// command (run in a separate goroutine) could have mutated the router
	// between ensureBinding and this point. Fall back to the passed snapshot
	// only if the binding was removed entirely.
	if fresh, ok := h.Router.Lookup(chatID); ok {
		binding = fresh
	}

	h.Logger.Debug("runPrompt start",
		log.FieldChatID, chatID,
		log.FieldSessionID, binding.SessionID,
		"prompt", truncateForDebug(prompt, h.debugRedact()))

	// Wrap parent with WithCancelCause so emitTerminal can distinguish a
	// user-initiated cancel (context.Canceled) from a prompt timeout
	// (context.DeadlineExceeded) via context.Cause. PromptTimeout defaults
	// to 0 (disabled) — the CLI exits on its own when the turn is done.
	ctx, cancel := context.WithCancelCause(parent)
	if h.PromptTimeout > 0 {
		timer := time.AfterFunc(h.PromptTimeout, func() {
			cancel(context.DeadlineExceeded)
		})
		defer timer.Stop()
	}
	// Idle watchdog: the timer is reset on every stdout event via onActivity
	// (wired through runOMP→streamRun). If the CLI goes silent for
	// IdleTimeout the timer fires cancel(errIdleTimeout), which SIGKILLs the
	// process group (ApplyGroupCancel) so streamRun unblocks and returns
	// isIdleTimeout — the user sees a "响应超时" notice instead of waiting
	// forever on a stuck subprocess. 0 disables it.
	var idleTimer *time.Timer
	if h.IdleTimeout > 0 {
		idleTimer = time.AfterFunc(h.IdleTimeout, func() {
			cancel(errIdleTimeout)
		})
	}
	defer func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
	}()
	onActivity := func() {
		if idleTimer != nil {
			idleTimer.Reset(h.IdleTimeout)
		}
	}
	defer cancel(nil)

	modelSpec := binding.ModelSpec
	opts := omp.RunOptions{
		Prompt:       prompt,
		Directory:    binding.Directory,
		SessionID:    binding.SessionID,
		Model:        modelSpec,
		ApprovalMode: mapApprovalMode(binding.PermissionMode, h.PermissionDefault),
		ThinkingLevel: mapThinkingLevel(binding.EffortLevel, h.thinkingDefault),
	}

	result := h.runOMP(ctx, chatID, replyToID, opts, modelSpec, onActivity)

	// Stale-session recovery (§10.7): if --resume hit a session the CLI no
	// longer knows, drop the binding's sessionID and retry once with a fresh
	// session. The exact报文 was confirmed empirically against omp/17.1.8:
	// a bad --resume id makes omp exit 1 with empty stdout and stderr
	// `Error: Session "<id>" not found.`, which the client's pump surfaces
	// as a synthesised EventError whose text contains "Session" + "not found".
	// isStaleSessionErr matches that signature so a real error (403, network,
	// …) with a non-empty SessionID does NOT trigger a spurious retry.
	if result.err != nil && !result.isCancelled && !result.isIdleTimeout &&
		binding.SessionID != "" && ctx.Err() == nil && isStaleSessionErr(result.err) {
		h.Logger.Warn("stale omp session, retrying without --resume",
			log.FieldChatID, chatID,
			log.FieldSessionID, binding.SessionID,
			log.FieldError, result.err)
		h.Router.SetSessionID(chatID, "")
		opts.SessionID = ""
		result = h.runOMP(ctx, chatID, replyToID, opts, modelSpec, onActivity)
	}

	// recordUsage before emitTerminal: emitTerminal reads the store to fill
	// the cumulative TotalTokens on the result card, so this turn must be
	// counted first. Add is an in-memory map update (the async save is
	// non-blocking), so this does not delay the terminal emit.
	h.recordUsage(chatID, result)
	h.emitTerminal(ctx, chatID, replyToID, result)
}

// recordUsage feeds the turn's token breakdown to the usage store. A
// cancelled turn is skipped: the subprocess was SIGKILLed and its terminal
// message_end (the source of these counts) typically did not arrive. Errors
// are still recorded — a failed run that consumed tokens is real cost.
func (h *Handler) recordUsage(chatID string, result promptResult) {
	if h.Usage == nil || result.isCancelled || result.sessionID == "" {
		return
	}
	h.Usage.Add(usage.Delta{
		SessionID:  result.sessionID,
		ChatID:     chatID,
		Input:      result.inputTokens,
		Output:     result.outputTokens,
		CacheRead:  result.cacheRead,
		CacheWrite: result.cacheWrite,
		Cost:       result.costUSD,
		Turns:      1,
	})
}

// runOMP starts one omp subprocess, streams its events into Controls, and
// reduces the stream to a promptResult. onActivity is wired through to
// streamRun so the idle watchdog in runPrompt resets per event.
func (h *Handler) runOMP(ctx context.Context, chatID, promptID string, opts omp.RunOptions, modelSpec string, onActivity func()) promptResult {
	// Archive the raw stream for this run before launching the subprocess so
	// the sink is wired for the whole lifetime. Best-effort: nil sink = off.
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, "omp", chatID, promptID, h.StreamHistory)
	if sink != nil {
		opts.LineSink = sink
		defer func() { _ = closeSink() }() // archive already flushed
	}

	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return promptResult{
			err:   fmt.Errorf("启动 omp 失败: %w", err),
			model: resolveModel("", modelSpec),
		}
	}
	return h.streamRun(ctx, chatID, promptID, events, modelSpec, onActivity)
}

// emitTerminal renders the terminal control for a finished turn: cancelled
// -> info notice, error -> error control, success -> result control. All
// branches use a fresh short-lived context (not the prompt ctx) so the
// terminal control still reaches the frontend when the prompt ctx is already
// cancelled (user abort, prompt timeout, or IPC blip during the turn).
func (h *Handler) emitTerminal(ctx context.Context, chatID, replyToID string, result promptResult) {
	sendCtx, cancel := context.WithTimeout(context.Background(), cancelNoticeTimeout)
	defer cancel()

	switch {
	case result.isIdleTimeout:
		h.emitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{
				Level:   "warning",
				Title:   "响应超时",
				Message: fmt.Sprintf("omp 已 %d 秒无输出，已终止", int(h.IdleTimeout.Seconds())),
			},
		})
	case result.isCancelled:
		title := "已取消"
		msg := "本次请求已中止"
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			title = "请求超时"
			msg = "omp 响应超时，已终止"
		}
		h.emitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "info", Title: title, Message: msg},
		})
	case result.err != nil:
		h.emitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeError,
			ChatID: chatID,
			Error:  &protocol.ErrorPayload{Message: result.err.Error()},
		})
	default:
		// Cumulative input+output across this session's turns (including
		// this one, already recorded by recordUsage above). 0 when no store
		// or no history; the renderer hides the cumulative portion then.
		var totalTokens int
		if e, ok := h.Usage.Get(result.sessionID); ok {
			totalTokens = e.Input + e.Output
		}
		h.emitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeResult,
			ChatID: chatID,
			Result: &protocol.ResultPayload{
				Text:        result.reply,
				Model:       result.model,
				Tokens:      result.contextTokens,
				Duration:    time.Duration(result.durationMs) * time.Millisecond,
				SessionID:   result.sessionID,
				Cost:        result.costUSD,
				Steps:       result.steps,
				TotalTokens: totalTokens,
			},
		})
	}
}

// mapApprovalMode resolves the omp --approval-mode value from the chat's
// permission pin. OMP's native values are always-ask|write|yolo; the /perm
// picker offers those directly, so binding.PermissionMode is usually already
// omp-native. The claude-name aliases (bypassPermissions/acceptEdits/plan)
// are mapped defensively so a config or binding migrated from claude-back
// still produces a valid omp flag (§7.2). Empty falls back to def
// (PermissionDefault, itself omp-native from cfg.OMP.ApprovalMode).
func mapApprovalMode(pin, def string) string {
	switch pin {
	case "bypassPermissions":
		return "yolo"
	case "acceptEdits":
		return "write"
	case "plan":
		return "always-ask"
	case "":
		return def
	default:
		// omp-native (always-ask/write/yolo) or an operator-customised
		// value: pass through verbatim.
		return pin
	}
}

// mapThinkingLevel resolves the omp --thinking value. claude's effort values
// (low/medium/high/xhigh/max) are accepted by omp verbatim; omp additionally
// allows off/minimal/auto. Empty pin falls back to def (thinkingDefault),
// then to "auto" (§7.2).
func mapThinkingLevel(pin, def string) string {
	if pin != "" {
		return pin
	}
	if def != "" {
		return def
	}
	return "auto"
}

// isStaleSessionErr reports whether err looks like omp's "session not found"
// failure on a bad --resume. Confirmed signature (omp/17.1.8, exit 1, empty
// stdout): stderr is `Error: Session "<id>" not found.`, which the client's
// pump rolls into the synthesised EventError text alongside the exit status.
// Matching on "session" + "not found" (case-insensitive) keeps this stable
// across id values and the surrounding "Run `omp --resume` …" hint without
// over-matching unrelated errors (a 403 / network error never contains both).
func isStaleSessionErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "session") && strings.Contains(s, "not found")
}
