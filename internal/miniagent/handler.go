package miniagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
)

// controlSender is the subset of *backendrpc.Client the handler needs.
// Exists so tests substitute a fake capturing Controls instead of POSTing.
type controlSender interface {
	SendControl(ctx context.Context, ctrl *protocol.Control) error
}

// Handler owns the per-process agent state: the LLM client, the emit
// channel, and the loop config derived from config.MiniAgent. One Handler
// per process; each turn runs on its own goroutine.
type Handler struct {
	llm    Client
	cfg    LoopConfig
	rpc    controlSender
	logger *log.Logger
}

// New wires the handler. llm is the LLM client (HTTPClient in production,
// Fake in tests). rpc emits Controls back to the frontend.
func New(llm Client, cfg LoopConfig, rpc controlSender, logger *log.Logger) *Handler {
	if logger == nil {
		logger = log.Nop()
	}
	return &Handler{llm: llm, cfg: cfg, rpc: rpc, logger: logger}
}

// HandleEvent dispatches Prompt events. Each prompt launches runTurn on its
// own goroutine (the SSE event loop must not block on a multi-second LLM
// call). ctx is the process-lifetime ctx from backendrpc.Run; it is NOT
// per-prompt (P0 has no /abort), so a turn only cancels on process shutdown.
// promptID MUST come from ev.PromptID (frontend assigns one per inbound
// message); reusing a chatID-derived id made the 2nd turn's Result collide
// with the 1st's closed card and silently drop.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		h.logger.Debug("miniagent ignore non-prompt event", "event_type", ev.Type)
		return nil
	}
	chatID := ev.Prompt.ChatID
	promptID := ev.PromptID
	prompt := strings.TrimSpace(ev.Prompt.Text)
	h.logger.Info("miniagent prompt received",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"prompt_len", len(prompt))
	if chatID == "" {
		return fmt.Errorf("miniagent: prompt missing chatID")
	}
	if promptID == "" {
		return fmt.Errorf("miniagent: prompt missing promptID (frontend must assign one per message)")
	}
	if prompt == "" {
		h.logger.Info("miniagent empty prompt, noticing", log.FieldChatID, chatID)
		return h.notify(ctx, chatID, "warning", "空消息", "请发送需要处理的内容。")
	}

	go h.runTurn(ctx, promptID, chatID, prompt)
	return nil
}

// runTurn runs the agent loop and emits the terminal Control. Always emits
// exactly one terminal Control (Result on success, Error on failure) so the
// frontend closes the turn card. A fresh short-lived ctx is used for the
// terminal emit so it still lands after the prompt ctx is cancelled.
func (h *Handler) runTurn(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	h.logger.Info("miniagent turn start",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"model", h.cfg.Model,
		"prompt_preview", truncateForLog(prompt, 80))

	result, err := Run(ctx, h.llm, h.cfg, promptID, prompt, h.emitHook(chatID, promptID), h.logger)
	if err != nil {
		h.logger.Warn("miniagent turn failed",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			log.FieldError, err,
			"duration", time.Since(start))
		h.emitError(chatID, promptID, err.Error())
		return
	}
	h.logger.Info("miniagent turn done",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"steps", result.Steps,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"duration", time.Since(start))

	emitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.rpc.SendControl(emitCtx, &protocol.Control{
		Type:     protocol.TypeResult,
		PromptID: promptID,
		ChatID:   chatID,
		Result: &protocol.ResultPayload{
			Text:        result.Text,
			Model:       h.cfg.Model,
			Tokens:      result.Usage.InputTokens + result.Usage.OutputTokens,
			Duration:    time.Since(start),
			TotalTokens: result.Usage.InputTokens + result.Usage.OutputTokens,
			SessionID:   "", // P0 is stateless; P2 memory supplies this.
		},
	}); err != nil {
		h.logger.Warn("miniagent emit result failed",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldError, err)
	}
}

// emitHook returns an EmitFunc that turns loop tool signals into frontend
// Controls (TypeToolUse when the LLM asks for a tool, TypeToolResult after
// execution) so the user sees the agent working. Both use the turn's
// promptID so the frontend folds them into the same card. Emits are
// best-effort: a failure is logged but never fails the turn.
func (h *Handler) emitHook(chatID, promptID string) EmitFunc {
	return func(_ string, sig Signal) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var ctrl *protocol.Control
		switch sig.Kind {
		case SignalToolUse:
			ctrl = &protocol.Control{
				Type:     protocol.TypeToolUse,
				PromptID: promptID,
				ChatID:   chatID,
				ToolUse:  &protocol.ToolUsePayload{Name: sig.Name, Input: sig.Input},
			}
		case SignalToolResult:
			ctrl = &protocol.Control{
				Type:       protocol.TypeToolResult,
				PromptID:   promptID,
				ChatID:     chatID,
				ToolResult: &protocol.ToolResultPayload{Name: sig.Name, Input: sig.Input, Output: sig.Output, IsError: sig.IsError},
			}
		default:
			h.logger.Debug("miniagent unknown signal kind", "kind", sig.Kind)
			return
		}
		if err := h.rpc.SendControl(ctx, ctrl); err != nil {
			h.logger.Warn("miniagent emit signal failed",
				log.FieldChatID, chatID, log.FieldPromptID, promptID,
				"kind", sig.Kind, log.FieldError, err)
		}
	}
}

// emitError sends a terminal TypeError so the frontend surfaces the failure
// instead of leaving the turn card hanging.
func (h *Handler) emitError(chatID, promptID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeError,
		PromptID: promptID,
		ChatID:   chatID,
		Error:    &protocol.ErrorPayload{Message: message, Recoverable: true},
	}); err != nil {
		h.logger.Warn("miniagent emit error failed",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldError, err)
	}
}

// notify emits a non-terminal Notice (e.g. empty-prompt warning).
func (h *Handler) notify(ctx context.Context, chatID, level, title, message string) error {
	return h.rpc.SendControl(ctx, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: message},
	})
}

// truncateForLog clamps a string to n runes for log previews so a long
// prompt does not flood the journal.
func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

