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
	model, workdir, perm := h.activeTurnConfig(ctx, chatID)
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
		StateDir:   h.stateDir,
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
			"incomplete", ev.Incomplete,
			log.FieldDuration, time.Since(start).Milliseconds())
		text := ev.Text
		// When the CLI hit its iteration cap, Text is empty and the user would
		// see a blank reply. Surface a brief explanation so the silence is
		// self-explaining; the Incomplete flag lets the frontend render a
		// dedicated style on top of this text if it wants.
		if ev.Incomplete && text == "" {
			text = "⏱️ 已达单轮最大步数（20）被截断，未生成最终回答。可重试或换更具体的提问。"
		}
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeResult,
			PromptID: promptID,
			ChatID:   chatID,
			Result: &protocol.ResultPayload{
				Text:        text,
				Model:       ev.Model,
				Tokens:      ev.InputTokens + ev.OutputTokens,
				Duration:    time.Since(start),
				Steps:       ev.Steps,
				TotalTokens: ev.InputTokens + ev.OutputTokens,
				Incomplete:  ev.Incomplete,
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

// activeTurnConfig returns the (model, workdir, permission) triple the CLI
// subprocess should be invoked with for this chat. The per-chat pin (held in
// the CLI's MetaStore) wins; missing pins fall back to the bridge's global
// defaults from config. One -show-current fork per turn; the ~5ms cost is
// negligible next to the LLM call that follows.
//
// When cli is nil (stateless mode — no state-dir configured), the global
// defaults are returned directly with no fork.
func (h *Handler) activeTurnConfig(ctx context.Context, chatID string) (model, workdir, permission string) {
	if h.cli == nil {
		return h.cfgModel, h.workspaceRoot, h.cfgPermission
	}
	state, err := h.cli.ShowCurrent(ctx, chatID)
	if err != nil {
		// Show-current failing does not prevent the turn: fall back to globals
		// and log so the operator can see the CLI is misconfigured.
		h.logger.Warn("miniagent: show-current failed, using globals", log.FieldChatID, chatID, log.FieldError, err)
		return h.cfgModel, h.workspaceRoot, h.cfgPermission
	}
	model = state.Model
	if model == "" {
		model = h.cfgModel
	}
	workdir = state.Directory
	if workdir == "" {
		workdir = h.workspaceRoot
	}
	permission = state.Permission
	if permission == "" {
		permission = h.cfgPermission
	}
	return model, workdir, permission
}

// activeModel/activeDir/activePermission are kept for command-side reads
// (/current display). They always go through cli.ShowCurrent; the ctx
// comes from the dispatcher's per-command timeout.
func (h *Handler) activeModel(ctx context.Context, chatID string) string {
	if h.cli == nil {
		return h.cfgModel
	}
	state, _ := h.cli.ShowCurrent(ctx, chatID)
	if state.Model != "" {
		return state.Model
	}
	return h.cfgModel
}

func (h *Handler) activePermission(ctx context.Context, chatID string) string {
	if h.cli == nil {
		return h.cfgPermission
	}
	state, _ := h.cli.ShowCurrent(ctx, chatID)
	if state.Permission != "" {
		return state.Permission
	}
	return h.cfgPermission
}
