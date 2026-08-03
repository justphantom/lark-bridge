package bridgebase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/usage"
)

// CancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled. Kept as a shared constant
// so every CLI bridge's EmitTerminal applies the same budget.
const CancelNoticeTimeout = 5 * time.Second

// messageIdleTimeout formats the per-backend idle-timeout message that
// matches the historical wording ("omp 已 N 秒无输出，已终止"). Kept as a
// helper so EmitTerminal stays a single switch.
func messageIdleTimeout(backendName string, idleTimeoutSec int) string {
	return fmt.Sprintf("%s 已 %d 秒无输出，已终止", backendName, idleTimeoutSec)
}

// PromptResult is the value a stream loop delivers once a CLI agent turn
// finishes (success, error, or cancellation). All three CLI bridges
// (claude/opencode/omp) had a near-identical local copy; promoting it lets
// RecordUsage / EmitTerminal live in the shared Core without a per-bridge
// adapter.
//
// CacheWrite vs CacheCreation: claude's stream-json calls the cache-creation
// token count `cache_creation_input_tokens`; opencode/omp call it `cacheWrite`
// (camelCase NDJSON). To keep one shape, both fields exist — claude fills
// CacheCreation, opencode/omp fill CacheWrite; RecordUsage collapses the
// difference (CacheWrite wins if non-zero, else CacheCreation).
type PromptResult struct {
	Reply        string
	Err          error
	Model        string
	SessionID    string
	DurationMs   int64
	ContextToken int // input+output (non-cache) shown on the result card
	CostUSD      float64
	Steps        int
	IsCancelled  bool
	// IsIdleTimeout is true when the turn was aborted by the idle watchdog.
	// Distinct from IsCancelled so EmitTerminal shows "响应超时" instead of
	// the generic "已取消". Currently only opencode/omp set this.
	IsIdleTimeout bool
	// Stale marks a "session no longer exists" error so runPrompt can drop the
	// binding's session id and retry once. Currently only claude sets this
	// (opencode does not have --resume; omp sets it via isStaleSessionErr at
	// runPrompt level, not on the result).
	Stale bool

	// Per-turn token breakdown fed to the usage store. CacheCreation and
	// CacheWrite are the same dimension under different CLI names; RecordUsage
	// collapses them into the usage.Delta's CacheWrite field.
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheWrite    int // opencode/omp name
	CacheCreation int // claude name; collapsed into CacheWrite by RecordUsage
}

// RecordUsage feeds the turn's token breakdown to the usage store. A
// cancelled turn is skipped: the subprocess was SIGKILLed and its terminal
// event (the source of these counts) typically did not arrive, so the numbers
// would be zero or stale. Errors are still recorded — a failed run that
// consumed tokens is real cost.
//
// Cache dimension collapse: callers that fill CacheCreation (claude) leave
// CacheWrite at 0; opencode/omp fill CacheWrite and leave CacheCreation at 0.
// Either way, the non-zero value lands in usage.Delta.CacheWrite.
func (c *Core) RecordUsage(chatID string, r PromptResult) {
	if c.Usage == nil || r.IsCancelled || r.SessionID == "" {
		return
	}
	cacheWrite := r.CacheWrite
	if cacheWrite == 0 {
		cacheWrite = r.CacheCreation
	}
	c.Usage.Add(usage.Delta{
		SessionID:  r.SessionID,
		ChatID:     chatID,
		Input:      r.InputTokens,
		Output:     r.OutputTokens,
		CacheRead:  r.CacheRead,
		CacheWrite: cacheWrite,
		Cost:       r.CostUSD,
		Turns:      1,
	})
}

// EmitTerminal renders the terminal control for a finished turn: cancelled
// → info notice, idle-timeout → warning notice, error → error control,
// success → result control. All branches use a fresh short-lived context
// (not the prompt ctx) so the terminal control still reaches the frontend
// when the prompt ctx is already cancelled (user abort, prompt timeout, or
// IPC blip during the turn).
//
// backendName personalises the cancel/timeout copy ("omp 已 N 秒无输出" vs
// "opencode 响应超时"), absorbing what was previously three ~40-line
// byte-similar functions.
// idleTimeoutSec is the configured IdleTimeout in seconds — passed in (rather
// than read from c.IdleTimeout) so backends without an idle watchdog can
// surface the right copy without wiring the field. Pass 0 if no idle timeout
// is configured; the idle branch is then unreachable (callers should also
// leave r.IsIdleTimeout false in that case).
//
// Returns an error only when the terminal control could not be delivered
// within the retry+ACK budget (see emitTerminalControl). The 3 CLI bridges'
// runPrompt callers use this to emit a last-resort fallback notice + bump the
// TerminalEmitLost counter, so a lost final reply is never silent.
func (c *Core) EmitTerminal(ctx context.Context, chatID, replyToID, backendName string, idleTimeoutSec int, r PromptResult) error {
	ctrl := c.buildTerminalControl(ctx, chatID, backendName, idleTimeoutSec, r)
	c.Logger.Info("terminal emit start",
		log.FieldChatID, chatID,
		log.FieldPromptID, replyToID,
		log.FieldControlType, ctrl.Type,
		"reply_len", len(r.Reply))
	start := time.Now()
	err := c.emitTerminalControl(replyToID, chatID, ctrl)
	c.Logger.Info("terminal emit done",
		log.FieldChatID, chatID,
		log.FieldPromptID, replyToID,
		log.FieldControlType, ctrl.Type,
		log.FieldDuration, time.Since(start).Milliseconds(),
		log.FieldError, err)
	return err
}

// buildTerminalControl renders the one terminal Control for the result branch
// (idle-timeout / cancelled / error / success). Extracted so emitTerminalControl
// can resend the SAME control on retry (idempotent frontend dedup keys on
// PromptID, so a duplicate is a harmless no-op there).
func (c *Core) buildTerminalControl(ctx context.Context, chatID, backendName string, idleTimeoutSec int, r PromptResult) *protocol.Control {
	switch {
	case r.IsIdleTimeout:
		msg := backendName + " 已无输出，已终止"
		if idleTimeoutSec > 0 {
			msg = messageIdleTimeout(backendName, idleTimeoutSec)
		}
		return &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "warning", Title: "响应超时", Message: msg},
		}
	case r.IsCancelled:
		title := "已取消"
		msg := "本次请求已中止"
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			title = "请求超时"
			msg = backendName + " 响应超时，已终止"
		}
		// A backend-initiated abort can carry a specific reason (e.g. omp's
		// auto-retry limit). Surface it instead of the generic copy. Plain
		// ctx errors (user abort / prompt timeout) keep the generic copy.
		if r.Err != nil && !errors.Is(r.Err, context.Canceled) && !errors.Is(r.Err, context.DeadlineExceeded) {
			msg = r.Err.Error()
		}
		return &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "info", Title: title, Message: msg},
		}
	case r.Err != nil:
		return &protocol.Control{
			Type:   protocol.TypeError,
			ChatID: chatID,
			Error:  &protocol.ErrorPayload{Message: r.Err.Error()},
		}
	}
	var totalTokens int
	if c.Usage != nil {
		if e, ok := c.Usage.Get(r.SessionID); ok {
			totalTokens = e.Input + e.Output
		}
	}
	return &protocol.Control{
		Type:   protocol.TypeResult,
		ChatID: chatID,
		Result: &protocol.ResultPayload{
			Text:        r.Reply,
			Model:       r.Model,
			Tokens:      r.ContextToken,
			Duration:    time.Duration(r.DurationMs) * time.Millisecond,
			SessionID:   r.SessionID,
			Cost:        r.CostUSD,
			Steps:       r.Steps,
			TotalTokens: totalTokens,
		},
	}
}

// emitTerminalControl sends c's terminal Control through the shared
// EmitTerminalControl retry+ACK loop. Thin wrapper so Core callers keep their
// c.emitTerminalControl(...) call site while the loop itself is shared with
// non-Core backends (miniagent) that have no Core to embed.
func (c *Core) emitTerminalControl(promptID, chatID string, ctrl *protocol.Control) error {
	var rpc backendrpc.ControlSender
	if c.RPC != nil {
		rpc = c.RPC // deref so a nil *backendrpc.Client stays a nil interface
	}
	return EmitTerminalControl(c.Logger, rpc, c.Acks, c.AppCtx, promptID, chatID, ctrl)
}

// EmitTerminalControl sends one terminal Control with a retry+ACK budget.
//
// The terminal control is the single most important message of a turn (the
// final reply / error / timeout notice). Unlike intermediate controls
// (EmitAsync, disposable), losing it strands the turn: the user never sees the
// reply and the progress card sits "in-flight" until a frontend restart. So
// this path trades a little latency for reliability:
//
//  1. Send the control (perAttemptTimeout budget).
//  2. Wait up to ackWaitBudget for the frontend's ACK (protocol.TypeAck,
//     carried on the SSE stream). The ACK resolves acks and the loop exits
//     early — success.
//  3. On timeout/no-ACK, retry (up to maxTerminalAttempts) with backoff.
//  4. If all attempts go unconfirmed, return the last error (or a synthetic
//     "no ACK" error); the caller emits a fallback notice and bumps
//     TerminalEmitLost.
//
// Extracted to a package-level helper (from Core.emitTerminalControl) so
// non-Core backends (miniagent) share the same retry reliability without
// embedding a Core:
//   - rpc performs the actual POST; nil → no-op success on attempt 1 (a
//     hand-built test Core with no IPC client wired, where nothing will ever
//     ACK either).
//   - acks pairs the emit with the frontend's delivery ACK; nil skips the ACK
//     wait (pure retry-on-send-error) — used by miniagent, which has no
//     AckRegistry. A successful POST is "delivered" on the first attempt
//     because a confirmation that can never arrive must not regress the happy
//     path.
//   - appCtx cancels the between-attempt backoff sleep on shutdown.
//
// ctrl.PromptID is back-filled from promptID when unset. Idempotent on the
// frontend (terminal dedup keys on PromptID), so a retry is a harmless no-op
// there if the first attempt did land.
func EmitTerminalControl(logger *log.Logger, rpc backendrpc.ControlSender, acks *AckRegistry, appCtx context.Context, promptID, chatID string, ctrl *protocol.Control) error {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	// No IPC client wired (hand-built test Core): nothing to send and nothing
	// to ever ACK, so a single no-op success is the correct "delivered" signal.
	if rpc == nil {
		return nil
	}
	var lastErr error
	for attempt := 1; attempt <= maxTerminalAttempts; attempt++ {
		if attempt > 1 {
			eventmetrics.TerminalEmitRetries.Inc()
			if logger != nil {
				logger.Warn("terminal control unconfirmed, retrying",
					log.FieldChatID, chatID, log.FieldPromptID, promptID,
					log.FieldControlType, ctrl.Type, "attempt", attempt)
			}
			select {
			case <-time.After(terminalRetryBackoff(attempt)):
			case <-appCtx.Done():
				return ctxOrLast(appCtx.Err(), lastErr)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), perAttemptTimeout)
		sendErr := rpc.SendControl(ctx, ctrl)
		cancel()
		if sendErr != nil {
			lastErr = sendErr
			if logger != nil {
				logger.Warn("terminal control send failed",
					log.FieldChatID, chatID, log.FieldPromptID, promptID,
					log.FieldControlType, ctrl.Type, log.FieldError, sendErr, "attempt", attempt)
			}
			continue
		}
		// Send accepted (202). No ACKer → pure-retry-on-send-error: a
		// successful POST with no ACK ingress is "delivered" (miniagent has no
		// AckRegistry; a nil rpc was handled above).
		if acks == nil {
			return nil
		}
		// Wait for the frontend's ACK. Resolve = success; timeout = retry.
		// ErrAckRegistryClosed = shutdown: stop retrying immediately — the
		// control was NOT confirmed delivered, so report it as undelivered
		// (pre-Close-drain this path returned success and inflated the ACK
		// metric while skipping the remaining attempts).
		err := acks.WaitFor(promptID, ackWaitBudget)
		acks.Forget(promptID)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrAckRegistryClosed) {
			return err
		}
		lastErr = fmt.Errorf("terminal control %s: no ACK within %s", ctrl.Type, ackWaitBudget)
	}
	return lastErr
}

// ctxOrLast returns ctxErr when non-nil (the shutdown cause), else lastErr so
// the caller sees the actionable send/ACK failure rather than a bare context.
func ctxOrLast(ctxErr, lastErr error) error {
	if ctxErr != nil {
		return ctxErr
	}
	return lastErr
}

// terminalRetryBackoff returns the sleep before the Nth retry (2<=attempt<=
// maxTerminalAttempts): 1s, 2s, 4s, ... capped at 4s. Attempt 1 does not sleep
// (the caller skips the backoff on the first try).
func terminalRetryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 2:
		return 1 * time.Second
	case attempt == 3:
		return 2 * time.Second
	default:
		return 4 * time.Second
	}
}

const (
	// maxTerminalAttempts bounds retries on the terminal control. 4 attempts
	// (1 initial + 3 retries) at the backoff ladder total ~7s of sleeps + up
	// to 4×5s send budgets + 4×6s ACK waits ≈ <60s worst case — well under the
	// user's perceived "stuck" threshold while tolerating a frontend blip.
	maxTerminalAttempts = 4
	// perAttemptTimeout bounds each SendControl HTTP POST.
	perAttemptTimeout = 5 * time.Second
	// ackWaitBudget bounds the wait for the frontend's ACK after a 202 accept.
	// Generous vs. a card render (<1s typical) but short enough that a
	// no-ACK frontend fails fast into the retry path instead of hanging.
	ackWaitBudget = 6 * time.Second
)

// HandleTerminalEmitError is the runPrompt epilogue shared by the CLI bridges
// (claude/opencode/omp): when EmitTerminal could not confirm delivery (all
// retries + ACK waits exhausted), it bumps TerminalEmitLost and emits a
// last-resort fallback notice so the turn is not silently stranded — the user
// sees that the reply is lost (not that the system hung). The fallback itself
// is best-effort: if the IPC path is truly down, even this notice may not
// arrive, but the metric is still bumped for observability.
//
// Call as: `if err := h.EmitTerminal(...); err != nil { bridgebase.HandleTerminalEmitError(h.Core, ctx, chatID, replyToID, err) }`.
// A nil err is a no-op.
func HandleTerminalEmitError(c *Core, ctx context.Context, chatID, replyToID string, emitErr error) {
	if emitErr == nil {
		return
	}
	eventmetrics.TerminalEmitLost.Inc()
	c.Logger.Error("terminal control delivery exhausted, turn result lost",
		log.FieldChatID, chatID,
		log.FieldPromptID, replyToID,
		log.FieldError, emitErr)
	// Best-effort fallback notice on a fresh context (the turn ctx may be
	// cancelled). Reuses the synchronous Emit, NOT emitTerminalControl, so a
	// broken IPC path does not loop again here — one shot, log on failure.
	fctx, cancel := context.WithTimeout(context.Background(), perAttemptTimeout)
	defer cancel()
	if err := c.Emit(fctx, replyToID, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{
			Level:   "warning",
			Title:   "回复投递失败",
			Message: "对话已完成但结果投递失败，请重新提问或使用 /use 恢复会话。",
		},
	}); err != nil {
		c.Logger.Warn("fallback notice also failed",
			log.FieldChatID, chatID, log.FieldPromptID, replyToID, log.FieldError, err)
	}
}
