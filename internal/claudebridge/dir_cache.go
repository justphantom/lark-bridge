package claudebridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/cmdutil"
)

// workspaceCacheTTL bounds how long a workspace subdir scan stays cached.
// Subdirectories change rarely (a user adds a project occasionally), so a
// short TTL keeps the picker fresh without forking a scan on every /cd.
const workspaceCacheTTL = 60 * time.Second

// dirListCache holds a snapshot of workspaceRoot's immediate subdirectories.
type dirListCache struct {
	dirs      []string
	fetchedAt time.Time
}

// listWorkspaceDirs returns the absolute paths of immediate subdirectories
// under the configured workspaceRoot, sorted by name. Results are cached for
// workspaceCacheTTL. An empty workspaceRoot yields an error so the caller
// (runDirPicker) surfaces "not configured" to the user.
func (h *Handler) listWorkspaceDirs() ([]string, error) {
	if h.workspaceRoot == "" {
		return nil, fmt.Errorf("未配置 WORKSPACE_ROOT 环境变量")
	}
	now := time.Now()
	h.workspaceMu.Lock()
	if h.workspaceCache != nil && now.Sub(h.workspaceCache.fetchedAt) < workspaceCacheTTL {
		out := h.workspaceCache.dirs
		h.workspaceMu.Unlock()
		return out, nil
	}
	h.workspaceMu.Unlock()

	entries, err := os.ReadDir(h.workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("读取 workspace 目录失败：%w", err)
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(h.workspaceRoot, e.Name()))
		}
	}
	sort.Slice(dirs, func(i, j int) bool {
		return filepath.Base(dirs[i]) < filepath.Base(dirs[j])
	})
	snapshot := make([]string, len(dirs))
	copy(snapshot, dirs)
	h.workspaceMu.Lock()
	h.workspaceCache = &dirListCache{dirs: snapshot, fetchedAt: time.Now()}
	h.workspaceMu.Unlock()
	return dirs, nil
}

// validateWorkspacePath checks that dir is an immediate or nested
// subdirectory of workspaceRoot, refusing escapes. An empty workspaceRoot
// refuses everything (the operator has not opted into /cd selection). The
// check uses filepath.Rel: a result starting with ".." escapes the root.
func validateWorkspacePath(dir, workspaceRoot string) error {
	if workspaceRoot == "" {
		return fmt.Errorf("未配置 WORKSPACE_ROOT 环境变量，无法校验目录")
	}
	cleaned := filepath.Clean(dir)
	root := filepath.Clean(workspaceRoot)
	rel, err := filepath.Rel(root, cleaned)
	if err != nil {
		return fmt.Errorf("目录不在 workspace 范围内：%s", dir)
	}
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("目录不在 workspace 范围内（%s 不在 %s 下）：%s", dir, workspaceRoot, dir)
	}
	return nil
}

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
	if err := validateWorkspacePath(dir, h.workspaceRoot); err != nil {
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
	h.router.SetDirectory(chatID, dir)
	h.router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.logger, chatID, "directory", dir)
	return cmdutil.ChangeResult("工作目录", b.Directory, dir, "会话已重置，下次提问生效。"), nil
}

// runDirPicker drives the interactive directory selection. It lists
// WORKSPACE_ROOT subdirectories, shows their basenames as options (no custom
// input — selection is restricted to listed dirs), and pins the chosen
// directory's full path.
func (h *Handler) runDirPicker(chatID, oldDir string) commandResult {
	dirs, err := h.listWorkspaceDirs()
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

	choice, err := h.askAndWait(chatID, "选择工作目录", options, false)
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
	h.router.SetDirectory(chatID, dir)
	h.router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.logger, chatID, "directory", dir)
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
	h.router.SetDirectory(chatID, "")
	h.router.SetSessionID(chatID, "")
	cmdutil.LogSettingChange(h.logger, chatID, "directory", "")
	return cmdutil.ChangeResult("工作目录", old, "(默认)", "已清除目录设置，会话已重置，下次提问生效。")
}
