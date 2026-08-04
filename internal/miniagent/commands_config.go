package miniagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// cmdConfig pins, clears, or interactively selects the per-chat miniagent
// -config file. Forms:
//   - /config       : pop a picker listing miniagent.json / *-miniagent.json
//     under the config directory (default ~/.miniagent).
//   - /config clear : restore the startup default config path.
//
// A free-form path argument is intentionally NOT accepted (mirrors claude
// /settings): the file must come from the directory scan so only files an
// operator placed there are selectable — no path traversal, no arbitrary
// -config target. Switching config does NOT reset the session; the new file
// takes effect on the next prompt, same as /model and /mode.
func (h *Handler) cmdConfig(_ context.Context, chatID, arg string) (level, title, body string) {
	if arg != "" && arg != "clear" {
		return "warning", "用法错误", "不支持自定义路径；用法：/config（从列表选择）或 /config clear"
	}
	if arg == "clear" {
		h.ensureBinding(chatID)
		h.router.SetConfigFile(chatID, "")
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义配置文件，将使用全局默认 %s。", h.clientDefaultConfig())
	}
	// Picker: listConfigFiles is a local read and askAndWait blocks for a human
	// click; run off the turn goroutine like /model and /cd. context.Background
	// so a late click (minutes later) still lands; askAndWait bounds the wait
	// itself via askWaitTimeout.
	promptID := h.PromptIDForPickers(chatID)
	go func() { //nolint:gosec // G118: picker outlives the request ctx — the user's click may come minutes later
		paths, err := listConfigFiles(h.configDir)
		if err != nil {
			h.notifyWithPromptID(chatID, promptID, "error", "选择失败", "读取配置目录失败："+err.Error())
			return
		}
		if len(paths) == 0 {
			h.notifyWithPromptID(chatID, promptID, "warning", "无可选项",
				"配置目录下没有 miniagent.json 或 *-miniagent.json 文件。")
			return
		}
		options := make([]string, len(paths))
		byName := make(map[string]string, len(paths))
		for i, p := range paths {
			name := filepath.Base(p)
			options[i] = name
			byName[name] = p
		}
		choice, messageID, err := h.askAndWait(context.Background(), chatID, promptID, "配置文件", options)
		if err != nil {
			h.notifyWithPromptID(chatID, promptID, "warning", "选择失败", err.Error())
			return
		}
		path, ok := byName[choice]
		if !ok {
			h.notifyWithCardUpdate(chatID, messageID, "error", "选择无效", "未知的配置文件："+choice)
			return
		}
		h.ensureBinding(chatID)
		h.router.SetConfigFile(chatID, path)
		h.notifyWithCardUpdate(chatID, messageID, "success", "已切换配置文件", "已切换到 "+choice+"（下次提问生效）。")
	}()
	return "async", "", ""
}

// listConfigFiles scans dir for miniagent.json and *-miniagent.json, returning
// absolute paths sorted by basename. Mirrors the startup ResolveConfigPath scan
// (canonical name plus operator-named variants) so the picker offers exactly
// the files the CLI accepts. Returns nil for an empty dir; an error only when
// the directory cannot be read.
func listConfigFiles(dir string) ([]string, error) {
	if dir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name != "miniagent.json" && !strings.HasSuffix(name, "-miniagent.json") {
			continue
		}
		abs, err := filepath.Abs(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	sort.Slice(out, func(i, j int) bool {
		return filepath.Base(out[i]) < filepath.Base(out[j])
	})
	return out, nil
}
