package goosebridge

import (
	"context"
	"strings"

	"github.com/hu/lark-bridge/internal/cmdutil"
)

// cmdModel pins, clears, or interactively selects the model for the current
// chat. Forms:
//   - /model            → pop a selection card (options from config; with a
//     custom-input box for a model not listed)
//   - /model clear      → clear the pin (fall back to goose's default)
//   - /model <spec>     → pin <spec> directly (e.g. /model gpt-4o)
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
	h.router.SetModelSpec(chatID, spec)
	cmdutil.LogSettingChange(h.logger, chatID, "model", spec)
	return cmdutil.ChangeResult("模型", old, spec, "下次提问生效。"), nil
}

// runModelPicker drives the interactive model selection. It emits the Question
// card via askAndWait (which blocks in this dispatcher goroutine for the
// user's answer), pins the result, and emits a confirmation Notice. Returns
// Handled so the dispatcher skips its default Notice (the confirmation is
// already emitted by emitNotice, and the dispatcher's ctx may have expired).
func (h *Handler) runModelPicker(chatID, oldSpec string) commandResult {
	choice, err := h.askAndWait(chatID, "选择模型", h.modelOptions, true)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldSpec
	if old == "" {
		old = "默认"
	}
	h.router.SetModelSpec(chatID, choice)
	cmdutil.LogSettingChange(h.logger, chatID, "model", choice)
	res := cmdutil.ChangeResult("模型", old, choice, "下次提问生效。")
	h.emitNoticeLogged(chatID, "success", "已切换模型", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearModelSpec is the /model clear path.
func clearModelSpec(h *Handler, chatID, oldSpec string) commandResult {
	old := oldSpec
	if old == "" {
		old = "默认"
	}
	h.router.SetModelSpec(chatID, "")
	cmdutil.LogSettingChange(h.logger, chatID, "model", "")
	return cmdutil.ChangeResult("模型", old, "默认", "已清除模型设置，将使用 goose 默认模型。")
}

// cmdHelp returns the auto-generated command list.
func (*Handler) cmdHelp(_ context.Context, _ string, _ []string) (commandResult, error) {
	return commandResult{Body: renderCmdHelp()}, nil
}
