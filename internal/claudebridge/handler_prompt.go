package claudebridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/streamarchive"
	"github.com/justphantom/lark-bridge/internal/strutil"
	"github.com/justphantom/lark-bridge/internal/usage"
)

// cancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled.
const cancelNoticeTimeout = 5 * time.Second

// runPrompt drives one Claude turn for chatID: it starts a `claude` CLI
// subprocess, streams its events, and emits the terminal control. The
// system/init session id is back-filled onto the binding so the next turn
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
	defer cancel(nil)

	modelSpec := binding.ModelSpec
	opts := claude.RunOptions{
		Prompt:         prompt,
		Directory:      binding.Directory,
		SessionID:      binding.SessionID,
		Model:          modelSpec,
		PermissionMode: binding.PermissionMode,
		EffortLevel:    binding.EffortLevel,
		SettingsFile:   strutil.ExpandEnvVars(binding.SettingsFile),
	}

	result := h.runClaude(ctx, chatID, replyToID, opts, modelSpec)

	// Stale-session recovery: if --resume hit a session the CLI no longer
	// knows, drop the binding's sessionID and retry once with a fresh session.
	if result.err != nil && binding.SessionID != "" &&
		strings.Contains(result.err.Error(), "No conversation found") &&
		ctx.Err() == nil {
		h.Logger.Warn("stale claude session, retrying without --resume",
			log.FieldChatID, chatID,
			log.FieldSessionID, binding.SessionID)
		h.Router.SetSessionID(chatID, "")
		opts.SessionID = ""
		result = h.runClaude(ctx, chatID, replyToID, opts, modelSpec)
	}

	// recordUsage before emitTerminal: emitTerminal reads the store to fill
	// the cumulative TotalTokens on the result card, so this turn must be
	// counted first. Add is an in-memory map update (the async save is
	// non-blocking), so this does not delay the terminal emit.
	h.recordUsage(chatID, result)
	h.emitTerminal(ctx, chatID, replyToID, result)
}

// recordUsage feeds the turn's token breakdown to the usage store. A cancelled
// turn is skipped: the subprocess was SIGKILLed and its result event (the only
// source of these counts) typically did not arrive, so the numbers would be
// zero or stale. Errors are still recorded — a failed run that consumed tokens
// is real cost.
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
		CacheWrite: result.cacheCreation,
		Cost:       result.costUSD,
		Turns:      1,
	})
}

// runClaude starts one Claude subprocess, streams its events into Controls,
// and reduces the stream to a promptResult.
func (h *Handler) runClaude(ctx context.Context, chatID, replyToID string, opts claude.RunOptions, modelSpec string) promptResult {
	// Archive the raw stream for this run before launching the subprocess so
	// the sink is wired for the whole lifetime. Best-effort: nil sink = off.
	// The sink is wrapped to drop thinking_tokens lines: the bridge never
	// consumes them (event_parse.go classifies them as inert EventSystem) yet
	// they dominate the claude archive volume (~88% of lines), so keeping them
	// bloats replay/debug material with no functional value. closeSink still
	// targets the underlying file; the wrapper is transparent to its lifecycle.
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, backendTag, chatID, replyToID, h.StreamHistory)
	if sink != nil {
		opts.LineSink = wrapThinkingFilter(sink)
		defer closeSink()
	}

	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return promptResult{
			err:   fmt.Errorf("启动 Claude 失败: %w", err),
			model: modelSpec,
		}
	}
	return h.streamRun(ctx, chatID, replyToID, events, modelSpec)
}

// emitTerminal renders the terminal control for a finished turn: cancelled
// → info notice, error → error control, success → result control. All
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
			msg = "Claude 响应超时，已终止"
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

