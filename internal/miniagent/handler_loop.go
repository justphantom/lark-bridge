package miniagent

import (
	"context"
	"errors"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// runViaLoop runs the ReAct loop in-process (the original mode before P3,
// still used by tests).
func (h *Handler) runViaLoop(ctx context.Context, promptID, chatID, prompt string) {
	start := time.Now()
	// Load history for this chat (nil on first turn or when memory is off).
	// The loop sees prior turns and returns the new messages in result.History.
	hist := h.history.Load(chatID)
	// Resolve per-chat overrides: .model pin and .dir pin both override the
	// global defaults. LoopConfig is a value type; cfg.Tools is rebuilt only
	// when dir differs from workspace_root (toolsForDir zero-allocs otherwise).
	cfg := h.cfg
	cfg.Model = h.activeModel(chatID)
	dir := h.activeDir(chatID)
	cfg.Tools = h.buildTools(chatID, dir)
	// Inject relevant long-term memory into the system prompt. We load chat
	// facts always, and project/global facts when a workspace is pinned.
	cfg.MemoryContext = h.memoryContext(chatID, dir)
	h.logger.Info("miniagent turn start",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"model", cfg.Model,
		"workdir", dir,
		"history_msgs", len(hist),
		"prompt_preview", truncate(prompt, 80, "…"))

	result, err := Run(ctx, h.llm, cfg, promptID, prompt, hist, h.emitHook(chatID, promptID), h.logger)
	if err != nil {
		// ctx.Canceled means the turn was aborted (user /session-abort or
		// Close). Surface as an info notice rather than a scary error; the
		// turn produced no History, so nothing is appended (the err path
		// returns before the Append below), keeping aborted turns out of the
		// conversation log.
		if errors.Is(err, context.Canceled) {
			h.logger.Info("miniagent turn aborted",
				log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldDuration, time.Since(start).Milliseconds())
			h.sendCtrl(&protocol.Control{
				Type:     protocol.TypeNotice,
				PromptID: promptID,
				ChatID:   chatID,
				Notice:   &protocol.NoticePayload{Level: "info", Title: "已中止", Message: "本次任务已停止。"},
			})
			return
		}
		h.logger.Warn("miniagent turn failed",
			log.FieldChatID, chatID,
			log.FieldPromptID, promptID,
			log.FieldError, err,
			log.FieldDuration, time.Since(start).Milliseconds())
		h.sendCtrl(&protocol.Control{
			Type:     protocol.TypeError,
			PromptID: promptID,
			ChatID:   chatID,
			Error:    &protocol.ErrorPayload{Message: err.Error(), Recoverable: true},
		})
		return
	}
	h.logger.Info("miniagent turn done",
		log.FieldChatID, chatID,
		log.FieldPromptID, promptID,
		"steps", result.Steps,
		"input_tokens", result.Usage.InputTokens,
		"output_tokens", result.Usage.OutputTokens,
		log.FieldDuration, time.Since(start).Milliseconds())

	// Persist this turn's new messages so the next turn remembers context.
	// The file is append-only (old turns stay on disk); Load trims what the
	// LLM actually sees, so unbounded growth only costs disk, not context.
	h.history.Append(chatID, result.History)

	h.sendCtrl(&protocol.Control{
		Type:     protocol.TypeResult,
		PromptID: promptID,
		ChatID:   chatID,
		Result: &protocol.ResultPayload{
			Text:        result.Text,
			Model:       cfg.Model,
			Tokens:      result.Usage.InputTokens + result.Usage.OutputTokens,
			Duration:    time.Since(start),
			Steps:       result.Steps,
			TotalTokens: result.Usage.InputTokens + result.Usage.OutputTokens,
			SessionID:   h.history.Current(chatID), // "" when memory is off / not yet created
		},
	})
}

// emitHook returns an EmitFunc that turns loop tool signals into frontend
// Controls (TypeToolUse when the LLM asks for a tool, TypeToolResult after
// execution) so the user sees the agent working. Both use the turn's
// promptID so the frontend folds them into the same card. Emits are
// best-effort: a failure is logged but never fails the turn.
func (h *Handler) emitHook(chatID, promptID string) EmitFunc {
	return func(sig Signal) {
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
		h.sendCtrl(ctrl)
	}
}

// activeModel returns the model this chat should use: the per-chat pin
// (from .model file) if set, otherwise the global default (cfg.Model).
func (h *Handler) activeModel(chatID string) string {
	if m := h.history.Model(chatID); m != "" {
		return m
	}
	return h.cfg.Model
}

// activeDir returns the working directory this chat should use: the per-chat
// pin (from .dir file) if set, otherwise the global workspace_root.
func (h *Handler) activeDir(chatID string) string {
	if d := h.history.Directory(chatID); d != "" {
		return d
	}
	return h.workspaceRoot
}

// activePermission returns the permission mode this chat should use: the
// per-chat pin (from .perm file) if set, otherwise the global default.
func (h *Handler) activePermission(chatID string) string {
	if p := h.history.Permission(chatID); p != "" {
		return p
	}
	return h.cfgPermission
}

// buildTools returns the full tool set for one turn: workspace-bound tools
// scoped to dir and memory tools scoped to chatID. This always allocates a
// new slice because memory tools capture chatID; the cost is negligible
// compared to one LLM call.
func (h *Handler) buildTools(chatID, dir string) []Tool {
	base := h.cfg.Tools
	if dir != "" && dir != h.workspaceRoot {
		base = h.toolsForDir(dir)
	}
	out := make([]Tool, 0, len(base)+4)
	out = append(out, base...)
	out = append(out, NewMemoryTools(h.facts, chatID)...)
	return out
}

// toolsForDir returns a Tool slice with WorkspaceRoot-bearing tools cloned
// to use dir instead of the global root. WebFetch (no WorkspaceRoot) is
// passed through. If dir equals workspaceRoot, the original slice is returned
// unchanged (zero-alloc common path).
func (h *Handler) toolsForDir(dir string) []Tool {
	if dir == "" || dir == h.workspaceRoot {
		return h.cfg.Tools
	}
	out := make([]Tool, 0, len(h.cfg.Tools))
	for _, t := range h.cfg.Tools {
		switch v := t.(type) {
		case ReadFile:
			v.WorkspaceRoot = dir
			out = append(out, v)
		case WriteFile:
			v.WorkspaceRoot = dir
			out = append(out, v)
		case EditFile:
			v.WorkspaceRoot = dir
			out = append(out, v)
		case Shell:
			v.WorkspaceRoot = dir
			out = append(out, v)
		default:
			out = append(out, t) // WebFetch etc — no WorkspaceRoot
		}
	}
	return out
}

// memoryContext collects long-term facts to inject into the system prompt.
// Chat-scoped facts are always included; project/global facts are included
// when a workspace is available.
func (h *Handler) memoryContext(chatID, dir string) string {
	if h.facts == nil {
		return ""
	}
	var all []Fact
	if chat, _ := h.facts.List(ScopeChat, chatID, ""); len(chat) > 0 {
		all = append(all, chat...)
	}
	if dir != "" {
		if proj, _ := h.facts.List(ScopeProject, chatID, ""); len(proj) > 0 {
			all = append(all, proj...)
		}
	}
	if global, _ := h.facts.List(ScopeGlobal, chatID, ""); len(global) > 0 {
		all = append(all, global...)
	}
	return formatFacts(all)
}
