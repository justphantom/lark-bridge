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
// call). ctx cancels on process shutdown; P0 has no /abort command wired.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	if ev.Type != protocol.TypePrompt || ev.Prompt == nil {
		return nil
	}
	chatID := ev.Prompt.ChatID
	prompt := strings.TrimSpace(ev.Prompt.Text)
	if chatID == "" {
		return fmt.Errorf("miniagent: prompt missing chatID")
	}
	if prompt == "" {
		return h.notify(ctx, chatID, "warning", "空消息", "请发送需要处理的内容。")
	}

	// promptID correlates emits with this turn. P0 reuses the chatID-derived
	// id; the frontend groups Controls under one promptID into a card.
	promptID := promptIDFor(chatID)
	go h.runTurn(ctx, promptID, chatID, prompt)
	return nil
}

// runTurn runs the agent loop and emits the terminal Control. Always emits
// exactly one terminal Control (Result on success, Error on failure) so the
// frontend closes the turn card. A fresh short-lived ctx is used for the
// terminal emit so it still lands after the prompt ctx is cancelled.
func (h *Handler) runTurn(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	h.logger.Debug("miniagent turn start", log.FieldChatID, chatID, "prompt_len", len(prompt))

	result, err := Run(ctx, h.llm, h.cfg, promptID, prompt, nil)
	if err != nil {
		h.logger.Warn("miniagent turn failed", log.FieldChatID, chatID, log.FieldError, err)
		h.emitError(chatID, err.Error())
		return
	}
	h.logger.Info("miniagent turn done",
		log.FieldChatID, chatID,
		"steps", result.Steps,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		"duration", time.Since(start))

	emitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.rpc.SendControl(emitCtx, &protocol.Control{
		Type:     protocol.TypeResult,
		PromptID: promptID,
		ChatID:   chatID,
		Result: &protocol.ResultPayload{
			Text:           result.Text,
			Model:          h.cfg.Model,
			Tokens:         result.Usage.InputTokens + result.Usage.OutputTokens,
			Duration:       time.Since(start),
			TotalTokens:    result.Usage.InputTokens + result.Usage.OutputTokens,
			SessionID:      "", // P0 is stateless; P2 memory supplies this.
		},
	})
}

// emitError sends a terminal TypeError so the frontend surfaces the failure
// instead of leaving the turn card hanging.
func (h *Handler) emitError(chatID, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.rpc.SendControl(ctx, &protocol.Control{
		Type:   protocol.TypeError,
		ChatID: chatID,
		Error:  &protocol.ErrorPayload{Message: message, Recoverable: true},
	})
}

// notify emits a non-terminal Notice (e.g. empty-prompt warning).
func (h *Handler) notify(ctx context.Context, chatID, level, title, message string) error {
	return h.rpc.SendControl(ctx, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: message},
	})
}

// promptIDFor derives a stable per-turn id from chatID. The frontend groups
// Controls sharing a promptID into one card. P0 uses a fixed prefix so a
// chat has one rolling card; P2 (memory) may make this per-turn unique.
func promptIDFor(chatID string) string {
	return "miniagent:" + chatID
}
