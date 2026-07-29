package ompbridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// settableApprovalModes is the set of valid omp --approval-mode values the
// /perm command accepts directly. Mirrors the config default list; built as
// a set so an operator who trims the picker list still cannot pin an
// invalid mode via direct arg.
var settableApprovalModes = map[string]struct{}{
	"always-ask": {},
	"write":      {},
	"yolo":       {},
}

func isSettableApprovalMode(m string) bool {
	_, ok := settableApprovalModes[m]
	return ok
}

// settableThinkingLevels is the set of valid omp --thinking values the
// /thinking command accepts directly. Matches the config default list.
var settableThinkingLevels = map[string]struct{}{
	"off":     {},
	"minimal": {},
	"low":     {},
	"medium":  {},
	"high":    {},
	"xhigh":   {},
	"max":     {},
	"auto":    {},
}

func isSettableThinkingLevel(level string) bool {
	_, ok := settableThinkingLevels[level]
	return ok
}

// cmdModel pins, clears, or interactively selects the model for the current
// chat. Forms:
//   - /model            → pop a selection card (options from config; with a
//     custom-input box for a model not listed)
//   - /model clear      → clear the pin (fall back to omp's default)
//   - /model <spec>     → pin <spec> directly (e.g. /model glm-5.2)
//
// The spec is passed to the CLI as --model on the next run.
func (h *Handler) cmdModel(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runModelPicker(chatID, b.ModelSpec, bridgebase.ReplyToID(ctx)), nil
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

// runModelPicker drives the interactive model selection using the static
// config list (omp's `models --json` is too slow to drive a dynamic picker,
// §A.4). allowCustom=true so the user can type a model id not listed.
func (h *Handler) runModelPicker(chatID, oldSpec, replyToID string) commandResult {
	choice, messageID, err := h.AskAndWait(chatID, replyToID, "模型", "选择模型", bridgebase.StaticOptions(h.modelOptions), true)
	if err != nil {
		h.EmitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldSpec
	if old == "" {
		old = "默认"
	}
	h.Router.SetModelSpec(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "model", choice)
	res := cmdutil.ChangeResult("模型", old, choice, "下次提问生效。")
	h.EmitCardUpdateLogged(chatID, messageID, "success", "已切换模型", res.Body, res.Field, res.Before, res.After)
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
	return cmdutil.ChangeResult("模型", old, "默认", "已清除模型设置，将使用 omp 默认模型。")
}

// cmdPermission pins, clears, or interactively selects the omp approval mode
// (≈ claude permission_mode) for the current chat. Forms:
//   - /perm             → pop a selection card (options from config; no custom
//     input — selection is restricted to listed values)
//   - /perm clear       → clear the pin (fall back to the configured default)
//   - /perm <mode>      → pin <mode> directly (must be always-ask|write|yolo)
//
// No session reset is needed: approval mode is orthogonal to conversation
// context. Maps onto binding.PermissionMode (the router field the bridge
// reads in runPrompt's mapApprovalMode).
func (h *Handler) cmdPermission(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runPermPicker(chatID, b.PermissionMode, bridgebase.ReplyToID(ctx)), nil
	}
	if args[0] == "clear" {
		return clearApprovalMode(h, chatID, b.PermissionMode), nil
	}

	mode := strings.Join(args, " ")
	if !isSettableApprovalMode(mode) {
		return cmdutil.ErrorResult("未知审批模式 %q；可选 always-ask | write | yolo", mode)
	}
	old := b.PermissionMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, mode)
	cmdutil.LogSettingChange(h.Logger, chatID, "approval_mode", mode)
	return cmdutil.ChangeResult("审批模式", old, mode, "下次提问生效。"), nil
}

// runPermPicker is the approval analogue of runModelPicker. allowCustom=false
// so the picker restricts selection to the configured approval options.
func (h *Handler) runPermPicker(chatID, oldMode, replyToID string) commandResult {
	opts := make([]protocol.PermissionOption, len(h.approvalOptions))
	for i, o := range h.approvalOptions {
		opts[i] = protocol.PermissionOption{Label: o, Value: o}
	}
	choice, messageID, err := h.AskPermission(chatID, replyToID, "", "审批模式", protocol.PermissionMessage{Message: "选择审批模式"}, opts, true)
	if err != nil {
		h.EmitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	// Defensive: the picker card lists h.approvalOptions, and
	// isSettableApprovalMode is normally a superset of those, but an operator
	// can misconfigure approval_options to hold a value the CLI's
	// --approval-mode rejects. Persisting it would break the next omp run; reject
	// here (mirroring the /perm direct-arg path) and patch the picker card so
	// the user sees why nothing changed.
	if !isSettableApprovalMode(choice) {
		h.EmitCardUpdateLogged(chatID, messageID, "error", "选择无效",
			fmt.Sprintf("未知审批模式 %q；可选 always-ask | write | yolo", choice))
		return commandResult{Handled: true}
	}
	old := oldMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "approval_mode", choice)
	res := cmdutil.ChangeResult("审批模式", old, choice, "下次提问生效。")
	h.EmitCardUpdateLogged(chatID, messageID, "success", "已设置审批模式", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearApprovalMode is the /perm clear path.
func clearApprovalMode(h *Handler, chatID, oldMode string) commandResult {
	old := oldMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "approval_mode", "")
	return cmdutil.ChangeResult("审批模式", old, "默认 ("+h.PermissionDefault+")",
		"已清除审批设置，回退默认。")
}

// cmdThinking is generated by bridgebase.MakeEnumPicker (see commands.go init).
// The picker shape — pin / clear / interactively select from a static list
// with allowCustom=false — is byte-identical to claude's /effort, so the
// shared factory centralises the ~70 lines both backends used to spell
// inline. Setter wires the EffortLevel router field + the structured-log
// line. The displayed default reflects the omp-only thinkingDefault (kept
// on Handler, not in CoreConfig, to avoid name collision with claude's
// effort concept).
func (h *Handler) cmdThinking(ctx context.Context, chatID string, args []string) (commandResult, error) {
	return bridgebase.MakeEnumPicker(h.Core, bridgebase.EnumPickerConfig{
		Spec:       cmdutil.Spec{Name: "/thinking", Summary: "设置思考级别；不带参数弹出选择；传 clear 清除", Args: "[level|clear]", Title: "已设置思考级别", Level: "success"},
		FieldLabel: "思考级别",
		LogKey:     "thinking_level",
		Options:    h.thinkingOptions,
		Default:    h.thinkingDefault,
		ErrorHint:  "可选 off | minimal | low | medium | high | xhigh | max | auto",
		Valid:      isSettableThinkingLevel,
	}, bridgebase.EnumPickerAccessors[*Handler]{
		Ensure: func(h *Handler, chatID string) error {
			_, err := h.ensureBinding(chatID, "", "", "", "")
			return err
		},
		Get: func(h *Handler, chatID string) string {
			b, _ := h.Router.Lookup(chatID)
			return b.EffortLevel
		},
		Set: func(h *Handler, chatID, v string) {
			h.Router.SetEffortLevel(chatID, v)
			cmdutil.LogSettingChange(h.Logger, chatID, "thinking_level", v)
		},
	}).Handler(h, ctx, chatID, args)
}

// cmdDirectory is implemented in dir_cache.go alongside the workspace scan
// and validation helpers.
