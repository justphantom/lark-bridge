package claudebridge

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// settablePermissionModes is the subset of CLI --permission-mode values
// the /mode command accepts. "default" is intentionally excluded: it
// prompts interactively and would deadlock the non-interactive -p
// subprocess until prompt_timeout. Built from the claude package's
// canonical constants so the string values stay single-sourced.
var settablePermissionModes = map[string]struct{}{
	claude.PermissionModeAcceptEdits:       {},
	claude.PermissionModePlan:              {},
	claude.PermissionModeBypassPermissions: {},
}

func isSettablePermissionMode(m string) bool {
	_, ok := settablePermissionModes[m]
	return ok
}

// cmdMode pins, clears, or interactively selects the per-chat Claude
// permission mode. Forms:
//   - /mode             → pop a selection card (options from config; no custom
//     input — selection is restricted to listed values)
//   - /mode clear       → clear the pin (fall back to the configured default)
//   - /mode <mode>      → pin <mode> directly (must be a valid mode)
//
// No session reset is needed: permission mode is orthogonal to conversation
// context. "default" is rejected on the direct-pin path — it would hang the
// non-interactive stream subprocess.
func (h *Handler) cmdMode(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runModePicker(chatID, b.PermissionMode, bridgebase.ReplyToID(ctx)), nil
	}
	if args[0] == "clear" {
		return clearMode(h, chatID, b.PermissionMode), nil
	}

	mode := strings.Join(args, " ")
	if !isSettablePermissionMode(mode) {
		return cmdutil.ErrorResult("未知权限模式 %q；可选 acceptEdits | plan | bypassPermissions（不接受 default：会挂死流式子进程）", mode)
	}

	old := b.PermissionMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, mode)
	cmdutil.LogSettingChange(h.Logger, chatID, "permission_mode", mode)
	return cmdutil.ChangeResult("权限模式", old, mode, "下次提问生效。"), nil
}

// runModePicker is the permission analogue of runModelPicker. allowCustom=false
// so the picker restricts selection to the configured permission options.
func (h *Handler) runModePicker(chatID, oldMode, replyToID string) commandResult {
	opts := make([]protocol.PermissionOption, len(h.permissionOptions))
	for i, o := range h.permissionOptions {
		opts[i] = protocol.PermissionOption{Label: o, Value: o}
	}
	choice, messageID, err := h.AskPermission(chatID, replyToID, "", "权限模式", protocol.PermissionMessage{Message: "选择权限模式"}, opts, true)
	if err != nil {
		h.EmitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "permission_mode", choice)
	res := cmdutil.ChangeResult("权限模式", old, choice, "下次提问生效。")
	h.EmitCardUpdateLogged(chatID, messageID, "success", "已设置权限模式", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearMode is the /mode clear path.
func clearMode(h *Handler, chatID, oldMode string) commandResult {
	old := oldMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "permission", "")
	return cmdutil.ChangeResult("权限模式", old, "默认 ("+h.PermissionDefault+")",
		"已清除权限设置，回退默认。")
}

// cmdDirectory is implemented in dir_cache.go alongside the workspace scan
// and validation helpers.

// cmdSettings interactively selects or clears the Claude --settings file for
// the current chat. Forms:
//   - /config           → pop a selection card listing settings files found
//     in the settings directory (selection restricted to
//     listed files; no custom-input box)
//   - /config clear     → clear the pin
//
// A free-form path argument is intentionally NOT accepted: the file must come
// from the settings directory scan so only files an operator placed there are
// selectable. Changing the settings file does not reset the session.
func (h *Handler) cmdSettings(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runSettingsPicker(chatID, b.SettingsFile, bridgebase.ReplyToID(ctx)), nil
	}
	if args[0] == "clear" {
		return clearSettingsFile(h, chatID, b.SettingsFile), nil
	}
	return cmdutil.ErrorResult("不支持自定义路径；用法：/config（从列表选择）或 /config clear")
}

// runSettingsPicker drives the interactive settings-file selection. It lists
// settings files via the agent, shows their basenames as options, and pins the
// chosen file's full path. allowCustom=false: the user can only pick a listed
// file, so the pinned path always comes from the trusted settings-directory
// scan and never from free-form input. replyToID keeps the flow on the
// command's progress card (see runModelPicker); pre-answer failures terminate
// that card via emitPromptNotice, post-answer failures patch the picker card.
func (h *Handler) runSettingsPicker(chatID, oldFile, replyToID string) commandResult {
	paths, err := h.agent.ListSettings(h.AppCtx)
	if err != nil {
		h.EmitPromptNotice(chatID, replyToID, "error", "选择失败", "获取 settings 文件列表失败："+err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	if len(paths) == 0 {
		h.EmitPromptNotice(chatID, replyToID, "warning", "无可选项",
			"settings 目录下没有 settings.json 或 *-settings.json 文件。")
		return commandResult{Body: "没有可用的 settings 文件", Handled: true}
	}

	// Build basename options for the card and a name→path map for reverse
	// lookup after the user picks.
	options := make([]string, len(paths))
	byName := make(map[string]string, len(paths))
	for i, p := range paths {
		name := filepath.Base(p)
		options[i] = name
		byName[name] = p
	}

	choice, messageID, err := h.AskAndWait(chatID, replyToID, "settings 文件", "选择 settings 文件", bridgebase.StaticOptions(options), false)
	if err != nil {
		h.EmitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}

	// allowCustom=false → choice is a listed basename; resolve it to its full
	// path. An unknown value is a defensive reject (it should not happen).
	path, ok := byName[choice]
	if !ok {
		h.EmitCardUpdateLogged(chatID, messageID, "error", "选择无效", "未知的 settings 文件："+choice)
		return commandResult{Body: "未知的 settings 文件：" + choice, Handled: true}
	}
	old := oldFile
	if old == "" {
		old = "(未设置)"
	}
	h.Router.SetSettingsFile(chatID, path)
	cmdutil.LogSettingChange(h.Logger, chatID, "settings_file", path)
	res := cmdutil.ChangeResult("--settings 文件", old, path, "下次提问生效。")
	h.EmitCardUpdateLogged(chatID, messageID, "success", "已设置 settings 文件", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearSettingsFile is the /config clear path.
func clearSettingsFile(h *Handler, chatID, oldFile string) commandResult {
	old := oldFile
	if old == "" {
		old = "(未设置)"
	}
	h.Router.SetSettingsFile(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "settings_file", "")
	return cmdutil.ChangeResult("--settings 文件", old, "(未设置)", "已清除 --settings 文件设置。")
}
