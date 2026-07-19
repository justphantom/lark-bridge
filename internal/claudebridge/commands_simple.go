package claudebridge

import (
	"context"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// cmdModel pins, clears, or interactively selects the model for the current
// chat. Forms:
//   - /model            → pop a selection card (options from config; with a
//     custom-input box for a model not listed)
//   - /model clear      → clear the pin (fall back to Claude's default)
//   - /model <spec>     → pin <spec> directly (e.g. /model claude-sonnet-4-5)
//
// The spec is passed verbatim to the CLI as --model on the next run.
func (h *Handler) cmdModel(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runModelPicker(chatID, b.ModelSpec), nil
	}
	if args[0] == "clear" {
		return clearModelSpec(h, chatID, b.ModelSpec), nil
	}

	spec := strings.Join(args, " ")
	old := b.ModelSpec
	if old == "" {
		old = "默认"
	}
	h.Router.SetModelSpec(chatID, spec)
	cmdutil.LogSettingChange(h.Logger, chatID, "model", spec)
	return cmdutil.ChangeResult("模型", old, spec, "下次提问生效。"), nil
}

// runModelPicker drives the interactive model selection. It emits the Question
// card via askAndWait (which blocks in this dispatcher goroutine for the
// user's answer), pins the result, and emits a confirmation Notice. Returns
// Handled so the dispatcher skips its default Notice (the confirmation is
// already emitted by emitNotice, and the dispatcher's ctx may have expired).
func (h *Handler) runModelPicker(chatID, oldSpec string) commandResult {
	choice, messageID, err := h.AskAndWait(chatID, "", "模型", "选择模型", bridgebase.StaticOptions(h.modelOptions), true)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldSpec
	if old == "" {
		old = "默认"
	}
	h.Router.SetModelSpec(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "model", choice)
	res := cmdutil.ChangeResult("模型", old, choice, "下次提问生效。")
	h.emitCardUpdateLogged(chatID, messageID, "success", "已切换模型", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearModelSpec is the /model clear path.
func clearModelSpec(h *Handler, chatID, oldSpec string) commandResult {
	old := oldSpec
	if old == "" {
		old = "默认"
	}
	h.Router.SetModelSpec(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "model", "")
	return cmdutil.ChangeResult("模型", old, "默认", "已清除模型设置，将使用 Claude 默认模型。")
}

// settableEffortLevels is the set of valid --effort values the /effort
// command accepts. These map directly to Claude Code CLI effort levels.
var settableEffortLevels = map[string]struct{}{
	"low":    {},
	"medium": {},
	"high":   {},
	"xhigh":  {},
	"max":    {},
}

func isSettableEffortLevel(level string) bool {
	_, ok := settableEffortLevels[level]
	return ok
}

// cmdEffort pins, clears, or interactively selects the Claude effort level.
// Forms:
//   - /effort           → pop a selection card (options from config; no custom
//     input — selection is restricted to listed values)
//   - /effort clear     → clear the pin
//   - /effort <level>   → pin <level> directly (must be a valid level)
//
// The level is passed to the CLI as --effort on the next run. No session reset
// is needed since effort is orthogonal to conversation context.
func (h *Handler) cmdEffort(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runEffortPicker(chatID, b.EffortLevel), nil
	}
	if args[0] == "clear" {
		return clearEffortLevel(h, chatID, b.EffortLevel), nil
	}

	level := strings.Join(args, " ")
	if !isSettableEffortLevel(level) {
		return cmdutil.ErrorResult("未知推理级别 %q；可选 low | medium | high | xhigh | max", level)
	}

	old := b.EffortLevel
	if old == "" {
		old = "默认"
	}
	h.Router.SetEffortLevel(chatID, level)
	cmdutil.LogSettingChange(h.Logger, chatID, "effort_level", level)
	return cmdutil.ChangeResult("推理级别", old, level, "下次提问生效。"), nil
}

// runEffortPicker is the effort analogue of runModelPicker. allowCustom=false
// so the picker restricts selection to the configured effort options.
func (h *Handler) runEffortPicker(chatID, oldLevel string) commandResult {
	choice, messageID, err := h.AskAndWait(chatID, "", "推理级别", "选择推理级别", bridgebase.StaticOptions(h.effortOptions), false)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldLevel
	if old == "" {
		old = "默认"
	}
	h.Router.SetEffortLevel(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "effort_level", choice)
	res := cmdutil.ChangeResult("推理级别", old, choice, "下次提问生效。")
	h.emitCardUpdateLogged(chatID, messageID, "success", "已设置推理级别", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearEffortLevel is the /effort clear path.
func clearEffortLevel(h *Handler, chatID, oldLevel string) commandResult {
	old := oldLevel
	if old == "" {
		old = "默认"
	}
	h.Router.SetEffortLevel(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "effort", "")
	return cmdutil.ChangeResult("推理级别", old, "默认", "已清除推理级别设置，将使用 Claude 默认级别。")
}

// cmdHelp returns the auto-generated command list.
func (*Handler) cmdHelp(_ context.Context, _ string, _ []string) (commandResult, error) {
	return commandResult{Body: renderCmdHelp()}, nil
}
