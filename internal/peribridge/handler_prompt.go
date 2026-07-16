package peribridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/peri"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// promptCancel wraps a context.CancelFunc in a struct so the entry stored in
// cancelByChat is uniquely identifiable by pointer equality.
type promptCancel struct {
	cancel    context.CancelFunc
	startTime time.Time
	chatID    string
}

// cancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled.
const cancelNoticeTimeout = 5 * time.Second

// runPrompt drives one peri turn for chatID: it starts a peri CLI subprocess,
// streams its events, and emits the terminal control.
//
// peri is stateless: there is no session id to back-fill, so this is simpler
// than the opencode variant. There is also no usage recording (peri emits no
// token data).
func (h *Handler) runPrompt(parent context.Context, chatID string, binding router.Binding, prompt, replyToID string, mine *promptCancel) {
	defer func() {
		if r := recover(); r != nil {
			h.logger.Error("panic in runPrompt",
				log.FieldChatID, chatID,
				log.FieldPanic, r,
				log.FieldStack, debug.Stack())
			if h.appCtx.Err() == nil {
				h.emitLogged(context.Background(), replyToID, chatID, &protocol.Control{
					Type:   protocol.TypeNotice,
					ChatID: chatID,
					Notice: &protocol.NoticePayload{Level: "error", Title: "内部错误", Message: "⚠️ 内部错误，已恢复"},
				})
			}
		}
	}()
	defer h.wg.Done()
	defer h.endPrompt(chatID, mine)
	defer mine.cancel()

	// Re-read the binding: a concurrent /cd or /model command (run in a
	// separate goroutine) could have mutated the router between
	// ensureBinding and this point.
	if fresh, ok := h.router.Lookup(chatID); ok {
		binding = fresh
	}

	h.logger.Debug("runPrompt start",
		log.FieldChatID, chatID,
		"prompt", truncateForDebug(prompt, h.debugRedact()))

	ctx, cancel := context.WithCancelCause(parent)
	if h.promptTimeout > 0 {
		timer := time.AfterFunc(h.promptTimeout, func() {
			cancel(context.DeadlineExceeded)
		})
		defer timer.Stop()
	}
	defer cancel(nil)

	modelSpec := binding.ModelSpec
	opts := peri.RunOptions{
		Prompt:    prompt,
		Directory: binding.Directory,
		Model:     modelSpec,
	}

	result := h.runPeri(ctx, chatID, replyToID, opts, modelSpec)
	h.emitTerminal(ctx, chatID, replyToID, result)
}

// runPeri starts one peri subprocess, streams its events into Controls, and
// reduces the stream to a promptResult.
func (h *Handler) runPeri(ctx context.Context, chatID, promptID string, opts peri.RunOptions, modelSpec string) promptResult {
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
func (h *Handler) startPrompt(_ context.Context, chatID string) (ctx context.Context, mine *promptCancel, ok bool) {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if _, busy := h.cancelByChat[chatID]; busy {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(h.appCtx)
	mine = &promptCancel{
		cancel:    cancel,
		startTime: time.Now(),
		chatID:    chatID,
	}
	h.cancelByChat[chatID] = mine
	return ctx, mine, true
}

// endPrompt removes the per-chat cancel slot only if it still points at mine.
func (h *Handler) endPrompt(chatID string, mine *promptCancel) {
	if mine == nil {
		return
	}
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if cur, ok := h.cancelByChat[chatID]; ok && cur == mine {
		delete(h.cancelByChat, chatID)
	}
}

// abortChat cancels the in-flight prompt for chatID, if any.
func (h *Handler) abortChat(chatID string) bool {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if pc, ok := h.cancelByChat[chatID]; ok {
		pc.cancel()
		return true
	}
	return false
}
