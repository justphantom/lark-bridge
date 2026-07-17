package goosebridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/goose"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
	"github.com/hu/lark-bridge/internal/streamarchive"
	"github.com/hu/lark-bridge/internal/usage"
)

// cancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled.
const cancelNoticeTimeout = 5 * time.Second

// runPrompt drives one goose turn for chatID: it starts a goose CLI
// subprocess, streams its events, and emits the terminal control. The goose
// session anchor (--name) is the binding's SessionID: empty on the first
// turn (creates a session), non-empty afterwards (resumes it).
func (h *Handler) runPrompt(parent context.Context, chatID string, binding router.Binding, prompt, replyToID string, mine *bridgebase.PromptCancel) {
	defer func() {
		if r := recover(); r != nil {
			h.Logger.Error("panic in runPrompt",
				log.FieldChatID, chatID,
				log.FieldPanic, r,
				log.FieldStack, debug.Stack())
			if h.AppCtx.Err() == nil {
				h.emitLogged(context.Background(), replyToID, chatID, &protocol.Control{
					Type:   protocol.TypeNotice,
					ChatID: chatID,
					Notice: &protocol.NoticePayload{Level: "error", Title: "内部错误", Message: "⚠️ 内部错误，已恢复"},
				})
			}
		}
	}()
	defer h.Wg.Done()
	defer h.endPrompt(chatID, mine)
	defer mine.Cancel()

	// Re-read the binding: a concurrent /cd, /session-del or /model command
	// (run in a separate goroutine) could have mutated the router between
	// ensureBinding and this point.
	if fresh, ok := h.Router.Lookup(chatID); ok {
		binding = fresh
	}

	h.Logger.Debug("runPrompt start",
		log.FieldChatID, chatID,
		log.FieldSessionID, binding.SessionID,
		"prompt", truncateForDebug(prompt, h.debugRedact()))

	ctx, cancel := context.WithCancelCause(parent)
	if h.PromptTimeout > 0 {
		timer := time.AfterFunc(h.PromptTimeout, func() {
			cancel(context.DeadlineExceeded)
		})
		defer timer.Stop()
	}
	defer cancel(nil)

	modelSpec := binding.ModelSpec
	opts := goose.RunOptions{
		Prompt:      prompt,
		Directory:   binding.Directory,
		Model:       modelSpec,
		SessionName: binding.SessionID,
		// Resume=true only when the binding already has an anchor: the first
		// turn for a chat (anchor empty) creates the session, every subsequent
		// turn resumes it.
		Resume: binding.SessionID != "",
	}

	result := h.runGoose(ctx, chatID, replyToID, opts, modelSpec)

	// Stale-session recovery: goose reports "No session found with name '<X>'"
	// when the anchor was reset out-of-band (session DB wiped, name deleted).
	// Drop the anchor and retry once as a fresh create (Resume=false).
	if result.err != nil && opts.Resume &&
		strings.Contains(result.err.Error(), "No session found") &&
		ctx.Err() == nil {
		h.Logger.Warn("stale goose session, retrying without --resume",
			log.FieldChatID, chatID,
			log.FieldSessionID, binding.SessionID)
		h.Router.SetSessionID(chatID, "")
		opts.Resume = false
		opts.SessionName = ""
		result = h.runGoose(ctx, chatID, replyToID, opts, modelSpec)
	}

	h.recordUsage(chatID, result)
	h.emitTerminal(ctx, chatID, replyToID, result)
}

// recordUsage feeds the turn's token breakdown to the usage store. A
// cancelled turn is skipped: the subprocess was SIGKILLed and its complete
// event (the only source of these counts) typically did not arrive. Errors
// are still recorded — a failed run that consumed tokens is real cost.
func (h *Handler) recordUsage(chatID string, result promptResult) {
	if h.Usage == nil || result.isCancelled || result.sessionID == "" {
		return
	}
	h.Usage.Add(usage.Delta{
		SessionID: result.sessionID,
		ChatID:    chatID,
		Input:     result.inputTokens,
		Output:    result.outputTokens,
		Turns:     1,
	})
}

// runGoose starts one goose subprocess, streams its events into Controls,
// and reduces the stream to a promptResult.
func (h *Handler) runGoose(ctx context.Context, chatID, replyToID string, opts goose.RunOptions, modelSpec string) promptResult {
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, "goose", chatID, replyToID, h.StreamHistory)
	if sink != nil {
		opts.LineSink = sink
		defer closeSink()
	}

	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return promptResult{
			err:   fmt.Errorf("启动 goose 失败: %w", err),
			model: modelSpec,
		}
	}
	return h.streamRun(ctx, chatID, replyToID, events, modelSpec, opts.SessionName)
}

// emitTerminal renders the terminal control for a finished turn: cancelled
// → info notice, error → error control, success → result control. Uses a
// fresh short-lived context so the terminal control reaches the frontend
// even when the prompt ctx is already cancelled.
func (h *Handler) emitTerminal(ctx context.Context, chatID, replyToID string, result promptResult) {
	sendCtx, cancel := context.WithTimeout(context.Background(), cancelNoticeTimeout)
	defer cancel()

	switch {
	case result.isCancelled:
		title := "已取消"
		msg := "本次请求已中止"
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			title = "请求超时"
			msg = "goose 响应超时，已终止"
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
		// one). 0 when no store or no history; the renderer hides it then.
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
				Duration:    time.Duration(result.durationMs) * time.Millisecond,
				SessionID:   result.sessionID,
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
