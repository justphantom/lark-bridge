package opencodebridge

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// cmdDirectory pins, clears, or interactively selects the working directory
// for the current chat. Forms:
//   - /cd            → pop a selection card listing WORKSPACE_ROOT
//     subdirectories (no custom input; selection restricted)
//   - /cd clear      → clear the pin (fall back to the per-chat default dir)
//   - /cd <path>     → pin <path> directly (must be under WORKSPACE_ROOT)
//
// Changing the directory resets the session: --session would otherwise resume
// a conversation referencing files under the old directory.
func (h *Handler) cmdDirectory(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runDirPicker(chatID, b.Directory, bridgebase.ReplyToID(ctx)), nil
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

	h.AbortChat(chatID)
	h.Router.SetDirectory(chatID, dir)
	h.Router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "directory", dir)
	return cmdutil.ChangeResult("工作目录", b.Directory, dir, "会话已重置，下次提问生效。"), nil
}

// runDirPicker drives the interactive directory selection. It lists
// WORKSPACE_ROOT subdirectories, shows their basenames as options (no custom
// input — selection is restricted to listed dirs), and pins the chosen
// directory's full path. Uses askAndWait's listFn form so the (cached) scan
// runs on the picker's own ctx, matching the opencode picker convention.
// replyToID keeps the flow on the command's progress card (see runModelPicker);
// pre-answer failures terminate that card via emitPromptNotice, post-answer
// failures patch the picker card.
func (h *Handler) runDirPicker(chatID, oldDir, replyToID string) commandResult {
	choice, messageID, err := h.AskAndWait(chatID, replyToID, "目录", "选择工作目录", func(_ context.Context) ([]string, error) {
		dirs, err := h.DirCache.List()
		if err != nil {
			return nil, err
		}
		names := make([]string, len(dirs))
		for i, d := range dirs {
			names[i] = filepath.Base(d)
		}
		return names, nil
	}, false)
	if err != nil {
		h.emitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	// Resolve the basename back to its full path. listWorkspaceDirs is the
	// source of truth; a re-scan is cheap and avoids a stale name→path map.
	dirs, err := h.DirCache.List()
	if err != nil {
		h.emitCardUpdateLogged(chatID, messageID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	dir := ""
	for _, d := range dirs {
		if filepath.Base(d) == choice {
			dir = d
			break
		}
	}
	if dir == "" {
		h.emitCardUpdateLogged(chatID, messageID, "error", "选择失败", "选中的目录已不存在")
		return commandResult{Body: "选中的目录已不存在", Handled: true}
	}
	// Switching to the same directory is a no-op: skip the session reset so
	// the user keeps their conversation context. Patch the picker card in
	// place rather than emitting a standalone info card.
	if dir == filepath.Clean(oldDir) {
		h.emitCardUpdateLogged(chatID, messageID, "info", "目录未变化", "选中的目录与当前一致，会话保留。")
		return commandResult{Handled: true}
	}
	old := oldDir
	if old == "" {
		old = "(默认)"
	}
	h.AbortChat(chatID)
	h.Router.SetDirectory(chatID, dir)
	h.Router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "directory", dir)
	res := cmdutil.ChangeResult("工作目录", old, dir, "会话已重置，下次提问生效。")
	h.emitCardUpdateLogged(chatID, messageID, "success", "已切换目录", res.Body, res.Field, res.Before, res.After)
	return commandResult{Handled: true}
}

// clearDirectory is the /cd clear path: remove the pin so the chat falls back
// to its per-chat default directory (defaultDirectory/<chatID>).
func clearDirectory(h *Handler, chatID, oldDir string) commandResult {
	old := oldDir
	if old == "" {
		old = "(默认)"
	}
	h.AbortChat(chatID)
	h.Router.SetDirectory(chatID, "")
	h.Router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "directory", "")
	return cmdutil.ChangeResult("工作目录", old, "(默认)", "已清除目录设置，会话已重置，下次提问生效。")
}
