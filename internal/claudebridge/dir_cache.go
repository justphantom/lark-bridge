package claudebridge

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/cmdutil"
)

// cmdDirectory pins, clears, or interactively selects the working directory
// for the current chat. Forms:
//   - /cd            → pop a selection card listing WORKSPACE_ROOT
//     subdirectories (no custom input; selection restricted)
//   - /cd clear      → clear the pin (fall back to the per-chat default dir)
//   - /cd <path>     → pin <path> directly (must be under WORKSPACE_ROOT)
//
// Changing the directory resets the session: --resume would otherwise
// resurface a conversation referencing files under the old directory.
func (h *Handler) cmdDirectory(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runDirPicker(chatID, b.Directory), nil
	}
	if args[0] == "clear" {
		return clearDirectory(h, chatID, b.Directory), nil
	}

	dir := filepath.Clean(strings.Join(args, " "))
	if err := h.DirCache.Validate(dir); err != nil {
		return cmdutil.ErrorResult("%v", err)
	}
	if err := validateAbsDir(dir); err != nil {
		return commandResult{Body: err.Error()}, err
	}

	// Switching to the same directory is a no-op: skip the session reset so
	// the user keeps their conversation context.
	if dir == filepath.Clean(b.Directory) {
		return commandResult{Body: "目录未变化，会话保留。"}, nil
	}

	h.abortChat(chatID)
	h.Router.SetDirectory(chatID, dir)
	h.Router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "directory", dir)
	return cmdutil.ChangeResult("工作目录", b.Directory, dir, "会话已重置，下次提问生效。"), nil
}

// runDirPicker drives the interactive directory selection. It lists
// WORKSPACE_ROOT subdirectories, shows their basenames as options (no custom
// input — selection is restricted to listed dirs), and pins the chosen
// directory's full path.
func (h *Handler) runDirPicker(chatID, oldDir string) commandResult {
	dirs, err := h.DirCache.List()
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	if len(dirs) == 0 {
		h.emitNoticeLogged(chatID, "warning", "无可选项",
			"WORKSPACE_ROOT 下没有子目录。")
		return commandResult{Body: "没有可用的目录", Handled: true}
	}

	options := make([]string, len(dirs))
	byName := make(map[string]string, len(dirs))
	for i, d := range dirs {
		name := filepath.Base(d)
		options[i] = name
		byName[name] = d
	}

	choice, err := h.AskAndWait(chatID, "", "目录", "选择工作目录", bridgebase.StaticOptions(options), false)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	dir := byName[choice]
	// Switching to the same directory is a no-op: skip the session reset so
	// the user keeps their conversation context.
	if dir == filepath.Clean(oldDir) {
		h.emitNoticeLogged(chatID, "info", "目录未变化", "选中的目录与当前一致，会话保留。")
		return commandResult{Handled: true}
	}
	old := oldDir
	if old == "" {
		old = "(默认)"
	}
	h.abortChat(chatID)
	h.Router.SetDirectory(chatID, dir)
	h.Router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "directory", dir)
	res := cmdutil.ChangeResult("工作目录", old, dir, "会话已重置，下次提问生效。")
	h.emitNoticeLogged(chatID, "success", "已切换目录", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearDirectory is the /cd clear path: remove the pin so the chat falls back
// to its per-chat default directory (defaultDirectory/<chatID>).
func clearDirectory(h *Handler, chatID, oldDir string) commandResult {
	old := oldDir
	if old == "" {
		old = "(默认)"
	}
	h.abortChat(chatID)
	h.Router.SetDirectory(chatID, "")
	h.Router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "directory", "")
	return cmdutil.ChangeResult("工作目录", old, "(默认)", "已清除目录设置，会话已重置，下次提问生效。")
}
