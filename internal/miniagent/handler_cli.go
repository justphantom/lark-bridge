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
// Controls. The CLI process owns the loop/tools/LLM/memory; the bridge
// owns IPC + per-chat config + command dispatch.
func (h *Handler) runViaCLI(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	model := h.activeModel(chatID)
	workdir := h.activeDir(chatID)
	perm := h.activePermission(chatID)
	h.logger.Info("miniagent turn start",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"model", model,
		"workdir", workdir,
		"permission", perm)

	events, err := h.client.Run(ctx, miniclient.RunOptions{
		Prompt:     prompt,
		Model:      model,
		Workdir:    workdir,
		ChatID:     chatID,
		StateDir:   h.historyDir,
		Permission: perm,
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
	// a friendly "已中止" notice, not a scary TypeError. Match runViaLoop's
	// behavior: ctx cancelled → TypeNotice, not TypeError.
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
	case miniclient.KindToolResult:
		h.sendCtrl(&protocol.Control{
			Type:       protocol.TypeToolResult,
			PromptID:   promptID,
			ChatID:     chatID,
			ToolResult: &protocol.ToolResultPayload{Name: ev.Name, Input: ev.Input, Output: ev.Output, IsError: ev.IsError},
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
