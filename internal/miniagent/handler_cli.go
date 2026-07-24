package miniagent

import (
	"context"
	"errors"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// runViaCLI forks miniagent per turn, pumps its NDJSON stdout into
// Controls. The CLI process owns the loop/tools/LLM call; the bridge
// owns IPC + per-chat binding (Directory/ModelSpec) + command dispatch.
func (h *Handler) runViaCLI(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	model, workdir := h.activeTurnConfig(chatID)
	h.logger.Info("miniagent turn start",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"model", model,
		"workdir", workdir)

	events, err := h.client.Run(ctx, miniclient.RunOptions{
		Prompt:  prompt,
		Model:   model,
		Workdir: workdir,
	})
	if err != nil {
		h.logger.Warn("miniagent start failed",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldError, err)
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: "启动 miniagent 失败：" + err.Error(), Recoverable: true},
		})
		return
	}

	var emittedTerminal bool
	for ev := range events {
		if ev.IsTerminal {
			emittedTerminal = true
		}
		h.emitCLIEvent(chatID, promptID, ev, start)
	}
	// If the CLI was killed by abort/close before emitting a terminal event,
	// the miniclient pump synthesizes a KindError — but the user should see
	// a friendly "已中止" notice, not a scary TypeError.
	if ctx.Err() != nil && !emittedTerminal {
		h.logger.Info("miniagent turn aborted (no terminal event)",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeNotice,
			PromptID: promptID,
			ChatID:   chatID,
			Notice:   &protocol.NoticePayload{Level: "info", Title: "已中止", Message: "本次任务已停止。"},
		})
	}
}

// emitCLIEvent translates one miniclient.Event into a protocol.Control and
// emits it to the frontend.
func (h *Handler) emitCLIEvent(chatID, promptID string, ev miniclient.Event, start time.Time) {
	switch ev.Kind {
	case miniclient.KindToolUse:
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeToolUse,
			PromptID: promptID,
			ChatID:   chatID,
			ToolUse:  &protocol.ToolUsePayload{Name: ev.Name, Input: ev.Input},
		})
	case miniclient.KindResult:
		h.logger.Info("miniagent turn done",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			"steps", ev.Steps,
			"input_tokens", ev.InputTokens,
			"output_tokens", ev.OutputTokens,
			log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeResult,
			PromptID: promptID,
			ChatID:   chatID,
			Result: &protocol.ResultPayload{
				Text:        ev.Text,
				Model:       ev.Model,
				Tokens:      ev.InputTokens + ev.OutputTokens,
				Duration:    time.Since(start),
				Steps:       ev.Steps,
				TotalTokens: ev.InputTokens + ev.OutputTokens,
			},
		})
	case miniclient.KindError:
		h.logger.Warn("miniagent turn failed",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			log.FieldError, errors.New(ev.Message),
			log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: ev.Message, Recoverable: true},
		})
	}
}

// activeTurnConfig returns the (model, workdir) the CLI subprocess should be
// invoked with for this chat. Per-chat binding fields (router.Lookup) win;
// empty fields fall back to the bridge's global defaults from config.
//
// When no binding exists the globals are returned directly — the binding is
// created lazily by /model or /cd, not by the first prompt (miniagent has no
// session to seed).
func (h *Handler) activeTurnConfig(chatID string) (model, workdir string) {
	model = h.cfgModel
	workdir = h.workspaceRoot
	if h.router == nil {
		return model, workdir
	}
	b, ok := h.router.Lookup(chatID)
	if !ok {
		return model, workdir
	}
	if b.ModelSpec != "" {
		model = b.ModelSpec
	}
	if b.Directory != "" {
		workdir = b.Directory
	}
	return model, workdir
}

// activeModel returns the model the CLI would be invoked with for this chat
// (used by /current and /models display). Same precedence as activeTurnConfig.
func (h *Handler) activeModel(chatID string) string {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.ModelSpec != "" {
			return b.ModelSpec
		}
	}
	return h.cfgModel
}

// activeDir returns the workdir the CLI would be invoked with for this chat
// (used by /current display).
func (h *Handler) activeDir(chatID string) string {
	if h.router != nil {
		if b, ok := h.router.Lookup(chatID); ok && b.Directory != "" {
			return b.Directory
		}
	}
	return h.workspaceRoot
}

// ensureBinding returns the binding for chatID, creating one on first use.
// Required because Router.SetModelSpec/SetDirectory are no-ops on missing
// bindings; /model and /cd must create the binding before mutating it.
func (h *Handler) ensureBinding(chatID string) {
	if h.router == nil {
		return
	}
	if _, ok := h.router.Lookup(chatID); ok {
		return
	}
	h.router.Bind(chatID, "", "", "", "", "")
}
