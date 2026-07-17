package peribridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hu/lark-bridge/internal/cmdutil"
)

// resolveSettingsDir resolves the peri settings directory. An absolute path is
// used verbatim; empty → ~/.peri; a relative path or ~/ prefix anchors at
// $HOME. Mirrors claude's resolution so config values like "~/.peri" work.
func (h *Handler) resolveSettingsDir() string {
	dir := h.settingsDir
	if filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dir
	}
	if dir == "" {
		return filepath.Join(home, ".peri")
	}
	if dir == "~" {
		return home
	}
	if strings.HasPrefix(dir, "~/") {
		return filepath.Join(home, dir[2:])
	}
	return filepath.Join(home, dir)
}

// listSettingsFiles returns the absolute paths of *.json files in the peri
// settings directory, sorted by filename. The peri CLI's --settings flag
// accepts an extra settings file or JSON string; this scan lists operator-
// placed JSON files as picker options.
func (h *Handler) listSettingsFiles() ([]string, error) {
	dir := h.resolveSettingsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("读取 settings 目录失败：%w", err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Slice(paths, func(i, j int) bool {
		return filepath.Base(paths[i]) < filepath.Base(paths[j])
	})
	return paths, nil
}

// cmdSettings interactively selects or clears the peri --settings file for the
// current chat. Forms:
//   - /settings          → pop a selection card listing *.json under the peri
//     settings directory (selection restricted to listed files; no custom input)
//   - /settings clear    → clear the pin
//
// A free-form path argument is intentionally NOT accepted: the file must come
// from the settings directory scan so only operator-placed files are
// selectable. Changing the settings file does not reset the (stateless) turn.
func (h *Handler) cmdSettings(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "")
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

// runSettingsPicker drives the interactive settings-file selection. It scans
// the peri settings dir, shows basenames as options, and pins the chosen
// file's full path. allowCustom=false: the user can only pick a listed file.
func (h *Handler) runSettingsPicker(chatID, oldFile string) commandResult {
	paths, err := h.listSettingsFiles()
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}
	if len(paths) == 0 {
		dir := h.resolveSettingsDir()
		h.emitNoticeLogged(chatID, "warning", "无可选项",
			"settings 目录（"+dir+"）下没有 .json 文件。")
		return commandResult{Body: "没有可用的 settings 文件", Handled: true}
	}

	options := make([]string, len(paths))
	byName := make(map[string]string, len(paths))
	for i, p := range paths {
		name := filepath.Base(p)
		options[i] = name
		byName[name] = p
	}

	choice, err := h.AskAndWait(chatID, "", "settings 文件", "选择 settings 文件", func(_ context.Context) ([]string, error) {
		return options, nil
	}, false)
	if err != nil {
		h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
		return commandResult{Body: err.Error(), Handled: true}
	}

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
	h.emitNoticeLogged(chatID, "success", "已设置 settings 文件", res.Body, res.Field, res.Before, res.After)
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
