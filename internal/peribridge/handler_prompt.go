package peribridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/peri"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
	"github.com/hu/lark-bridge/internal/streamarchive"
)

// cancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled.
const cancelNoticeTimeout = 5 * time.Second

// runPrompt drives one peri turn for chatID: it starts a peri CLI subprocess,
// streams its events, and emits the terminal control.
//
// peri is stateless: there is no session id to back-fill, so this is simpler
// than the opencode variant. There is also no usage recording (peri emits no
// token data).
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

	// Re-read the binding: a concurrent /cd or /model command (run in a
	// separate goroutine) could have mutated the router between
	// ensureBinding and this point.
	if fresh, ok := h.Router.Lookup(chatID); ok {
		binding = fresh
	}

	h.Logger.Debug("runPrompt start",
		log.FieldChatID, chatID,
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
	opts := peri.RunOptions{
		Prompt:         prompt,
		Directory:      binding.Directory,
		Model:          modelSpec,
		Effort:         binding.EffortLevel,
		PermissionMode: binding.PermissionMode,
		SettingsFile:   binding.SettingsFile,
	}

	result := h.runPeri(ctx, chatID, replyToID, opts, modelSpec)
	h.emitTerminal(ctx, chatID, replyToID, result)
}

// runPeri starts one peri subprocess, streams its events into Controls, and
// reduces the stream to a promptResult. The raw NDJSON is archived under
// {stateDir}/streams when StreamHistory > 0 (best-effort).
func (h *Handler) runPeri(ctx context.Context, chatID, promptID string, opts peri.RunOptions, modelSpec string) promptResult {
	sink, closeSink := streamarchive.NewSink(h.Logger, h.StateDir, "peri", chatID, promptID, h.StreamHistory)
	if sink != nil {
		opts.LineSink = sink
		defer closeSink()
	}
	events, err := h.agent.Run(ctx, opts)
	if err != nil {
		return promptResult{
			err:   fmt.Errorf("启动 peri 失败: %w", err),
			model: resolveModel(modelSpec),
		}
	}
	return h.streamRun(ctx, chatID, promptID, events, modelSpec)
}

// emitTerminal renders the terminal control for a finished turn: cancelled
// -> info notice, error -> error control, success -> result control.
func (h *Handler) emitTerminal(ctx context.Context, chatID, replyToID string, result promptResult) {
	sendCtx, cancel := context.WithTimeout(context.Background(), cancelNoticeTimeout)
	defer cancel()

	switch {
	case result.isCancelled:
		title := "已取消"
		msg := "本次请求已中止"
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			title = "请求超时"
			msg = "peri 响应超时，已终止"
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
		h.emitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeResult,
			ChatID: chatID,
			Result: &protocol.ResultPayload{
				Text:     result.reply,
				Model:    result.model,
				Duration: time.Duration(result.durationMs) * time.Millisecond,
				Steps:    result.steps,
				// Tokens/Cost/SessionID/TotalTokens are zero/empty: peri print
				// mode emits no usage data and has no session id.
			},
		})
	}
}

// startPrompt tries to register a per-chat prompt slot derived from appCtx.
// Busy-then-drop per chat.
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

// abortChat cancels the in-flight prompt for chatID, if any.
func (h *Handler) abortChat(chatID string) bool {
	h.CancelMu.Lock()
	defer h.CancelMu.Unlock()
	if pc, ok := h.CancelByChat[chatID]; ok {
		pc.Cancel()
		return true
	}
	return false
}
