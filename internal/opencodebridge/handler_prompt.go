package opencodebridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/opencode"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
	"github.com/hu/lark-bridge/internal/streamarchive"
	"github.com/hu/lark-bridge/internal/usage"
)

// cancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled.
const cancelNoticeTimeout = 5 * time.Second

// runPrompt drives one opencode turn for chatID: it starts an `opencode` CLI
// subprocess, streams its events, and emits the terminal control. The
// session.created session id is back-filled onto the binding so the next turn
// resumes it.
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
	defer h.endPrompt(chatID, mine)
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
	// to 0 (disabled) — the subprocess exits on its own when the task is
	// done.
	ctx, cancel := context.WithCancelCause(parent)
	if h.PromptTimeout > 0 {
		timer := time.AfterFunc(h.PromptTimeout, func() {
			cancel(context.DeadlineExceeded)
		})
		defer timer.Stop()
	}
	defer cancel(nil)

	modelSpec := binding.ModelSpec
	opts := opencode.RunOptions{
		Prompt:    prompt,
		Directory: binding.Directory,
		SessionID: binding.SessionID,
		Model:     modelSpec,
		Agent:     binding.Agent,
	}

	result := h.runOpencode(ctx, chatID, replyToID, opts, modelSpec)

	// recordUsage before emitTerminal: emitTerminal reads the store to fill
	// the cumulative TotalTokens on the result card, so this turn must be
	// counted first. Add is an in-memory map update (the async save is
	// non-blocking), so this does not delay the terminal emit.
	h.recordUsage(chatID, result)
	h.emitTerminal(ctx, chatID, replyToID, result)
}

// recordUsage feeds the turn's token breakdown to the usage store. A cancelled
// turn is skipped: the subprocess was SIGKILLed and its terminal step_finish
// (the source of these counts) typically did not arrive. Errors are still
// recorded — a failed run that consumed tokens is real cost.
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

// runOpencode starts one opencode subprocess, streams its events into
// Controls, and reduces the stream to a promptResult.
func (h *Handler) runOpencode(ctx context.Context, chatID, promptID string, opts opencode.RunOptions, modelSpec string) promptResult {
	// Archive the raw stream for this run before launching the subprocess so
	// the sink is wired for the whole lifetime. Best-effort: nil sink = off.
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, "opencode", chatID, promptID, h.StreamHistory)
	if sink != nil {
		opts.LineSink = sink
		defer closeSink()
	}

	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return promptResult{
			err:   fmt.Errorf("启动 opencode 失败: %w", err),
			model: resolveModel("", modelSpec),
		}
	}
	return h.streamRun(ctx, chatID, promptID, events, modelSpec)
}

// emitTerminal renders the terminal control for a finished turn: cancelled
// -> info notice, error -> error control, success -> result control. All
// branches use a fresh short-lived context (not the prompt ctx) so the
// terminal control still reaches the frontend when the prompt ctx is
// already cancelled (user abort, prompt timeout, or IPC blip during the
// turn).
func (h *Handler) emitTerminal(ctx context.Context, chatID, replyToID string, result promptResult) {
	sendCtx, cancel := context.WithTimeout(context.Background(), cancelNoticeTimeout)
	defer cancel()

	switch {
	case result.isCancelled:
		title := "已取消"
		msg := "本次请求已中止"
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			title = "请求超时"
			msg = "opencode 响应超时，已终止"
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
		// Cumulative input+output across this session's turns (including this
		// one, already recorded by recordUsage above). 0 when no store or no
		// history; the renderer hides the cumulative portion then.
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

// startPrompt tries to register a per-chat prompt slot derived from appCtx.
// Busy-then-drop per chat: if a prompt is already in-flight for chatID the
// new one is rejected (ok=false). ok=false callers MUST NOT touch the
// returned ctx/mine (both nil).
func (h *Handler) startPrompt(_ context.Context, chatID string) (ctx context.Context, mine *bridgebase.PromptCancel, ok bool) {
	h.CancelMu.Lock()
	defer h.CancelMu.Unlock()
	if _, busy := h.CancelByChat[chatID]; busy {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(h.AppCtx)
	mine = &bridgebase.PromptCancel{
		Cancel:    cancel,
		StartTime: time.Now(),
		ChatID:    chatID,
	}
	h.CancelByChat[chatID] = mine
	return ctx, mine, true
}

// endPrompt removes the per-chat cancel slot only if it still points at mine.
func (h *Handler) endPrompt(chatID string, mine *bridgebase.PromptCancel) {
	if mine == nil {
		return
	}
	h.CancelMu.Lock()
	defer h.CancelMu.Unlock()
	if cur, ok := h.CancelByChat[chatID]; ok && cur == mine {
		delete(h.CancelByChat, chatID)
	}
}

// abortChat cancels the in-flight prompt for chatID, if any. Returns whether
// one was cancelled. Used by the TypeAbort event.
func (h *Handler) abortChat(chatID string) bool {
	h.CancelMu.Lock()
	defer h.CancelMu.Unlock()
	if pc, ok := h.CancelByChat[chatID]; ok {
		pc.Cancel()
		return true
	}
	return false
}
