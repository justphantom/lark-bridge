package opencodeservebridge

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/usage"
)

// cancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled.
const cancelNoticeTimeout = 5 * time.Second

// runPrompt drives one opencode turn for chatID: it starts a turn via the
// SDK Run, streams its HighEvents, and emits the terminal control. The
// sessionID is captured from the first HighEvent and back-filled onto the
// binding so the next turn resumes it.
func (h *Handler) runPrompt(parent context.Context, chatID string, binding router.Binding, prompt, replyToID string, mine *bridgebase.PromptCancel) {
	// Recover so a panic in this goroutine never crashes the process.
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
	defer h.EndPrompt(chatID, mine)
	defer mine.Cancel()

	// Re-read the binding here rather than trusting the snapshot the caller
	// took in handlePromptEvent: a concurrent /cd, /session-del or /model
	// command (run in a separate goroutine) could have mutated the router
	// between ensureBinding and this point.
	if fresh, ok := h.Router.Lookup(chatID); ok {
		binding = fresh
	}

	h.Logger.Debug("runPrompt start",
		log.FieldChatID, chatID,
		log.FieldSessionID, binding.SessionID,
		"prompt", truncateForDebug(prompt, h.debugRedact()))

	// Wrap parent with WithCancelCause so emitTerminal can distinguish a
	// user-initiated cancel (context.Canceled) from a prompt timeout
	// (context.DeadlineExceeded) via context.Cause.
	ctx, cancel := context.WithCancelCause(parent)
	if h.PromptTimeout > 0 {
		timer := time.AfterFunc(h.PromptTimeout, func() {
			cancel(context.DeadlineExceeded)
		})
		defer timer.Stop()
	}
	defer cancel(nil)

	modelSpec := binding.ModelSpec
	opts := oc.RunOptions{
		Prompt:    prompt,
		SessionID: binding.SessionID,
		Location:  &oc.LocationRef{Directory: binding.Directory},
	}
	// v1 无 Switch 接口，Model/Agent 随每条 prompt 生效，故每 turn 都从
	// binding 传（空值表示用服务端默认）。
	if ref, err := parseModelSpec(modelSpec); err == nil && ref.ID != "" {
		opts.Model = &ref
	}
	if binding.Agent != "" {
		opts.Agent = binding.Agent
	}

	result := h.runOpencode(ctx, chatID, replyToID, opts, modelSpec)

	h.recordUsage(chatID, result)
	h.emitTerminal(ctx, chatID, replyToID, result)
}

// recordUsage feeds the turn's token breakdown to the usage store. A cancelled
// turn is skipped: the SDK's abort fires an Interrupt and the terminal
// step_finish (the source of these counts) typically did not arrive. Errors
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

// runOpencode starts one SDK Run, streams its HighEvents into Controls, and
// reduces the stream to a promptResult.
func (h *Handler) runOpencode(ctx context.Context, chatID, promptID string, opts oc.RunOptions, modelSpec string) promptResult {
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
