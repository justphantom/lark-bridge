package peribridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hu/lark-bridge/internal/cmdutil"
)

// cmdModel pins or clears the model alias for the current chat. Forms:
//   - /model clear   → clear the pin (fall back to ~/.peri/settings.json)
//   - /model <alias> → pin <alias> (e.g. /model sonnet), passed as --model
//
// There is no interactive picker: peri exposes no list subcommand, so the
// available aliases are whatever the user configured in ~/.peri/settings.json
// (provider.models.opus/sonnet/haiku). A typo'd alias fails on the next run.
func (h *Handler) cmdModel(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	if len(args) == 0 {
		return commandResult{Body: "用法：`/model <别名>` 设置（如 opus/sonnet/haiku），或 `/model clear` 清除。模型别名对应 ~/.peri/settings.json 中 provider.models 的配置。"}, nil
	}
	if args[0] == "clear" {
		old := b.ModelSpec
		if old == "" {
			old = "默认"
		}
		h.Router.SetModelSpec(chatID, "")
		cmdutil.LogSettingChange(h.Logger, chatID, "model", "")
		return cmdutil.ChangeResult("模型", old, "默认", "已清除模型设置，将使用 ~/.peri/settings.json 的默认配置。"), nil
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

// validateAbsDir checks that dir is an absolute, existing, writable directory.
// Used by /cd before pinning a path.
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
func validateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}

// validateSettingsPath guards the --settings file path against directory
// traversal. An empty path is allowed (clear semantics). A path that, after
// filepath.Clean, still starts with ".." would escape upward relative to the
// working directory and is rejected. Absolute paths and paths that Clean to a
// non-escaping relative are accepted; the peri CLI itself reports a missing
// or malformed file on the next run.
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

// settablePermissionModes is the subset of peri --permission-mode values the
// /perm command accepts. "default" is intentionally excluded: it enables HITL
// approval which deadlocks the non-interactive -p subprocess.
var settablePermissionModes = map[string]struct{}{
	"bypass":      {},
	"accept-edit": {},
	"auto-mode":   {},
}

func isSettablePermissionMode(m string) bool {
	_, ok := settablePermissionModes[m]
	return ok
}

// defaultPermissionOptions is the fallback when cfg.PermissionOptions is empty.
var defaultPermissionOptions = []string{"bypass", "accept-edit", "auto-mode"}

// cmdPermission pins, clears, or interactively selects the per-chat peri
// permission mode. Forms:
//   - /perm        → pop a selection card (options from config; no custom input)
//   - /perm clear  → clear the pin (fall back to the configured default bypass)
//   - /perm <mode> → pin <mode> directly (must be valid; default rejected)
func (h *Handler) cmdPermission(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "")
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
		return cmdutil.ErrorResult("未知权限模式 %q；可选 bypass | accept-edit | auto-mode（不接受 default：会挂死流式子进程）", mode)
	}
	old := b.PermissionMode
	if old == "" {
		old = "默认 (bypass)"
	}
	h.Router.SetPermissionMode(chatID, mode)
	cmdutil.LogSettingChange(h.Logger, chatID, "permission_mode", mode)
	return cmdutil.ChangeResult("权限模式", old, mode, "下次提问生效。"), nil
}

// runPermPicker drives the interactive permission-mode selection.
func (h *Handler) runPermPicker(chatID, oldMode string) commandResult {
	opts := h.permissionOptions
	if len(opts) == 0 {
		opts = defaultPermissionOptions
	}
	choice, err := h.askAndWait(chatID, "", "权限模式", "选择权限模式", func(_ context.Context) ([]string, error) {
		return opts, nil
	}, false)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	old := oldMode
	if old == "" {
		old = "默认 (bypass)"
	}
	h.Router.SetPermissionMode(chatID, choice)
	cmdutil.LogSettingChange(h.Logger, chatID, "permission_mode", choice)
	res := cmdutil.ChangeResult("权限模式", old, choice, "下次提问生效。")
	h.emitNoticeLogged(chatID, "success", "已设置权限模式", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearPermissionMode is the /perm clear path.
func clearPermissionMode(h *Handler, chatID, oldMode string) commandResult {
	old := oldMode
	if old == "" {
		old = "默认 (bypass)"
	}
	h.Router.SetPermissionMode(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "permission_mode", "")
	return cmdutil.ChangeResult("权限模式", old, "默认 (bypass)", "已清除权限设置，回退默认 bypass。")
}

// settableEffortLevels is the set of peri --effort values the /effort command
// accepts, matching peri's documented low/medium/high/max.
var settableEffortLevels = map[string]struct{}{
	"low":    {},
	"medium": {},
	"high":   {},
	"max":    {},
}

func isSettableEffortLevel(l string) bool {
	_, ok := settableEffortLevels[l]
	return ok
}

// defaultEffortOptions is the fallback when cfg.EffortOptions is empty.
var defaultEffortOptions = []string{"low", "medium", "high", "max"}

// cmdEffort pins, clears, or interactively selects the per-chat peri reasoning
// effort. Forms:
//   - /effort        → pop a selection card (options from config; no custom input)
//   - /effort clear  → clear the pin (fall back to the peri default)
//   - /effort <lvl>  → pin <lvl> directly (must be valid)
func (h *Handler) cmdEffort(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "")
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
		return cmdutil.ErrorResult("未知推理级别 %q；可选 low | medium | high | max", level)
	}
	old := b.EffortLevel
	if old == "" {
		old = "默认"
	}
	h.Router.SetEffortLevel(chatID, level)
	cmdutil.LogSettingChange(h.Logger, chatID, "effort_level", level)
	return cmdutil.ChangeResult("推理级别", old, level, "下次提问生效。"), nil
}

// runEffortPicker drives the interactive effort-level selection.
func (h *Handler) runEffortPicker(chatID, oldLevel string) commandResult {
	opts := h.effortOptions
	if len(opts) == 0 {
		opts = defaultEffortOptions
	}
	choice, err := h.askAndWait(chatID, "", "推理级别", "选择推理级别", func(_ context.Context) ([]string, error) {
		return opts, nil
	}, false)
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
	h.emitNoticeLogged(chatID, "success", "已设置推理级别", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearEffortLevel is the /effort clear path.
func clearEffortLevel(h *Handler, chatID, oldLevel string) commandResult {
	old := oldLevel
	if old == "" {
		old = "默认"
	}
	h.Router.SetEffortLevel(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "effort_level", "")
	return cmdutil.ChangeResult("推理级别", old, "默认", "已清除推理级别设置，将使用 peri 默认配置。")
}
