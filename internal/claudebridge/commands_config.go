package claudebridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/claude-go-sdk"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// settablePermissionModes is the subset of CLI --permission-mode values
// the /perm command accepts. "default" is intentionally excluded: it
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

// cmdPermission pins, clears, or interactively selects the per-chat Claude
// permission mode. Forms:
//   - /perm             → pop a selection card (options from config; no custom
//     input — selection is restricted to listed values)
//   - /perm clear       → clear the pin (fall back to the configured default)
//   - /perm <mode>      → pin <mode> directly (must be a valid mode)
//
// No session reset is needed: permission mode is orthogonal to conversation
// context. "default" is rejected on the direct-pin path — it would hang the
// non-interactive stream subprocess.
func (h *Handler) cmdPermission(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runPermPicker(chatID, b.PermissionMode), nil
	}
	if args[0] == "clear" {
		return clearPermissionMode(h, chatID, b.PermissionMode), nil
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

// runPermPicker is the permission analogue of runModelPicker. allowCustom=false
// so the picker restricts selection to the configured permission options.
func (h *Handler) runPermPicker(chatID, oldMode string) commandResult {
	choice, messageID, err := h.AskAndWait(chatID, "", "权限模式", "选择权限模式", bridgebase.StaticOptions(h.permissionOptions), false)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldMode
	if old == "" {
		old = "默认 (" + h.PermissionDefault + ")"
	}
	h.Router.SetPermissionMode(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "permission_mode", choice)
	res := cmdutil.ChangeResult("权限模式", old, choice, "下次提问生效。")
	h.emitCardUpdateLogged(chatID, messageID, "success", "已设置权限模式", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearPermissionMode is the /perm clear path.
func clearPermissionMode(h *Handler, chatID, oldMode string) commandResult {
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
//   - /settings          → pop a selection card listing settings files found
//     in the settings directory (selection restricted to
//     listed files; no custom-input box)
//   - /settings clear    → clear the pin
//
// A free-form path argument is intentionally NOT accepted: the file must come
// from the settings directory scan so only files an operator placed there are
// selectable. Changing the settings file does not reset the session.
func (h *Handler) cmdSettings(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runSettingsPicker(chatID, b.SettingsFile), nil
	}
	if args[0] == "clear" {
		return clearSettingsFile(h, chatID, b.SettingsFile), nil
	}
	return cmdutil.ErrorResult("不支持自定义路径；用法：/settings（从列表选择）或 /settings clear")
}

// runSettingsPicker drives the interactive settings-file selection. It lists
// settings files via the agent, shows their basenames as options, and pins the
// chosen file's full path. allowCustom=false: the user can only pick a listed
// file, so the pinned path always comes from the trusted settings-directory
// scan and never from free-form input.
func (h *Handler) runSettingsPicker(chatID, oldFile string) commandResult {
	paths, err := h.agent.ListSettings(h.AppCtx)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", "获取 settings 文件列表失败："+err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	if len(paths) == 0 {
		h.emitNoticeLogged(chatID, "warning", "无可选项",
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

	choice, messageID, err := h.AskAndWait(chatID, "", "settings 文件", "选择 settings 文件", bridgebase.StaticOptions(options), false)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}

	// allowCustom=false → choice is a listed basename; resolve it to its full
	// path. An unknown value is a defensive reject (it should not happen).
	path, ok := byName[choice]
	if !ok {
		h.emitNoticeLogged(chatID, "error", "选择无效", "未知的 settings 文件："+choice)
		return commandResult{Body: "未知的 settings 文件：" + choice, Handled: true}
	}
	old := oldFile
	if old == "" {
		old = "(未设置)"
	}
	h.Router.SetSettingsFile(chatID, path)
	cmdutil.LogSettingChange(h.Logger, chatID, "settings_file", path)
	res := cmdutil.ChangeResult("--settings 文件", old, path, "下次提问生效。")
	h.emitCardUpdateLogged(chatID, messageID, "success", "已设置 settings 文件", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearSettingsFile is the /settings clear path.
func clearSettingsFile(h *Handler, chatID, oldFile string) commandResult {
	old := oldFile
	if old == "" {
		old = "(未设置)"
	}
	h.Router.SetSettingsFile(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "settings_file", "")
	return cmdutil.ChangeResult("--settings 文件", old, "(未设置)", "已清除 --settings 文件设置。")
}

// validateAbsDir checks that dir is an absolute path, an existing
// directory, and writable by the current uid — the same uid the Claude
// subprocess will run as, so the probe result is authoritative. The
// writability check is what makes a systemd ReadWritePaths exclusion
// surface here (with a clear message) rather than mid-turn inside
// Claude's acceptEdits flow.
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
		return fmt.Errorf("目录不可写（可能被 systemd ReadWritePaths 排除或 Unix 权限不足）：%w", err)
	}
	_ = os.Remove(probe)
	return nil
}

// validateSessionDirPath checks the shape of a session directory the bridge is
// about to create from an Event-carried override: it must be an absolute path.
// Event.Directory is empty in production (the frontend never sets it), so this
// is defence in depth — the workspace boundary itself is enforced by /cd. A
// relative path is rejected so an untrusted Event cannot make the subprocess
// CWD relative to the process working directory. ".." is not checked: for an
// absolute path filepath.Clean resolves it away, so IsAbs is sufficient here.
// Existence is not required (unlike /cd's validateAbsDir) — ensureBinding
// creates the dir via MkdirAll on demand.
func validateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}

// validateSettingsPath guards the --settings file path against directory
// traversal. An empty path is allowed (clear semantics). A path that, after
// filepath.Clean, still starts with ".." would escape upward relative to the
// working directory (e.g. "../../etc/passwd") and is rejected. Absolute paths
// and paths that Clean to a non-escaping relative are accepted; the Claude CLI
// itself reports a missing or malformed file on the next run.
func validateSettingsPath(path string) error {
	if path == "" {
		return nil
	}
	cleaned := filepath.Clean(path)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("路径包含非法的目录穿越（..）：%s", path)
	}
	return nil
}
