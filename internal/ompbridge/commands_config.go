package ompbridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		h.emitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
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
		h.emitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "approval_mode", choice)
	res := cmdutil.ChangeResult("审批模式", old, choice, "下次提问生效。")
	h.emitCardUpdateLogged(chatID, messageID, "success", "已设置审批模式", res.Body, res.Field, res.Before, res.After)
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

// cmdThinking pins, clears, or interactively selects the omp thinking level
// (≈ claude effort). Forms:
//   - /thinking           → pop a selection card (options from config; no
//     custom input — selection restricted to listed values)
//   - /thinking clear     → clear the pin
//   - /thinking <level>   → pin <level> directly (must be a valid level)
//
// The level is passed to the CLI as --thinking on the next run. Maps onto
// binding.EffortLevel. No session reset is needed since thinking is
// orthogonal to conversation context.
func (h *Handler) cmdThinking(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runThinkingPicker(chatID, b.EffortLevel, bridgebase.ReplyToID(ctx)), nil
	}
	if args[0] == "clear" {
		return clearThinkingLevel(h, chatID, b.EffortLevel), nil
	}

	level := strings.Join(args, " ")
	if !isSettableThinkingLevel(level) {
		return cmdutil.ErrorResult("未知思考级别 %q；可选 off | minimal | low | medium | high | xhigh | max | auto", level)
	}
	old := b.EffortLevel
	if old == "" {
		old = "默认 (" + h.thinkingDefault + ")"
	}
	h.Router.SetEffortLevel(chatID, level)
	cmdutil.LogSettingChange(h.Logger, chatID, "thinking_level", level)
	return cmdutil.ChangeResult("思考级别", old, level, "下次提问生效。"), nil
}

// runThinkingPicker is the thinking analogue of runModelPicker.
// allowCustom=false so the picker restricts selection to the configured
// thinking options.
func (h *Handler) runThinkingPicker(chatID, oldLevel, replyToID string) commandResult {
	choice, messageID, err := h.AskAndWait(chatID, replyToID, "思考级别", "选择思考级别", bridgebase.StaticOptions(h.thinkingOptions), false)
	if err != nil {
		h.emitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldLevel
	if old == "" {
		old = "默认 (" + h.thinkingDefault + ")"
	}
	h.Router.SetEffortLevel(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "thinking_level", choice)
	res := cmdutil.ChangeResult("思考级别", old, choice, "下次提问生效。")
	h.emitCardUpdateLogged(chatID, messageID, "success", "已设置思考级别", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearThinkingLevel is the /thinking clear path.
func clearThinkingLevel(h *Handler, chatID, oldLevel string) commandResult {
	old := oldLevel
	if old == "" {
		old = "默认 (" + h.thinkingDefault + ")"
	}
	h.Router.SetEffortLevel(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "thinking_level", "")
	return cmdutil.ChangeResult("思考级别", old, "默认 ("+h.thinkingDefault+")",
		"已清除思考级别设置，将使用 omp 默认级别。")
}

// cmdDirectory is implemented in dir_cache.go alongside the workspace scan
// and validation helpers.

// validateAbsDir checks that dir is an absolute path, an existing directory,
// and writable by the current uid — the same uid the omp subprocess will run
// as, so the probe result is authoritative.
func validateAbsDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("目录不可访问：%w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("路径不是目录：%s", dir)
	}

	probe, err := os.MkdirTemp(dir, ".cdprobe-*")
	if err != nil {
		return fmt.Errorf("目录不可写（权限不足）：%w", err)
	}
	_ = os.Remove(probe)
	return nil
}

// validateSessionDirPath checks the shape of a session directory the bridge is
// about to create from an Event-carried override: it must be an absolute path.
// Event.Directory is empty in production (the frontend never sets it), so this
// is defence in depth — the workspace boundary is enforced by /cd.
//
// IsAbs only, by design: a relative path (including "..") does not begin with
// "/", so IsAbs already rejects it; a ".." segment inside an absolute path
// (e.g. "/a/../b") is resolved by the filesystem to a concrete path at
// MkdirAll/CWD time and is not a traversal escape. Existence is not required
// (unlike /cd's validateAbsDir) — ensureBinding creates the dir via MkdirAll
// on demand.
func validateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}
