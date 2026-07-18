package miniagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdModel pins or clears the per-chat model:
//
//	/model            → show current + usage
//	/model clear      → clear pin (fall back to global default)
//	/model <id>       → pin <id> for this chat
func (h *Handler) cmdModel(chatID, arg string) (level, title, body string) {
	if h.history == nil {
		return "warning", "模型", "记忆未启用，无法设置模型。"
	}
	if arg == "" {
		// No arg → interactive picker (async, like opencode-back's runModelPicker).
		// ListModels may take tens of seconds, and askAndWait blocks for a human
		// click; both must run off the SSE event loop. Emit a "loading" notice
		// immediately, then launch a goroutine that fetches models, emits a
		// selection card, waits for the user's click, and applies it.
		lister := h.modelLister
		if lister == nil {
			return "warning", "模型", "当前 LLM 客户端不支持列模型。用 /model <ID> 直接指定。"
		}
		h.sendCtrl(&protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "info", Title: "正在加载模型列表", Message: "正在获取可用模型，请稍候…"},
		})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			models, err := lister.ListModels(ctx)
			if err != nil {
				h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "error", "选择失败", "获取模型列表失败："+err.Error())
				return
			}
			choice, err := h.askAndWait(ctx, chatID, "模型", models)
			if err != nil {
				h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "warning", "选择失败", err.Error())
				return
			}
			if err := h.history.SetModel(chatID, choice); err != nil {
				h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "error", "模型", "设置失败："+err.Error())
				return
			}
			h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "success", "已切换模型", "已切换到模型 "+choice+"（下次提问生效）。")
		}()
		return "async", "", "" // sentinel: handleSessionCommand must not notify
	}
	if arg == "clear" {
		if err := h.history.SetModel(chatID, ""); err != nil {
			return "error", "模型", "清除模型失败：" + err.Error()
		}
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义模型，将使用全局默认 %s。", h.cfg.Model)
	}
	if err := h.history.SetModel(chatID, arg); err != nil {
		return "error", "模型", "设置模型失败：" + err.Error()
	}
	return "success", "已切换模型", fmt.Sprintf("已切换到模型 %s（下次提问生效）。", arg)
}

// cmdModels lists available models from the LLM endpoint (GET /v1/models).
func (h *Handler) cmdModels(chatID, _ string) (level, title, body string) {
	lister := h.modelLister
	if lister == nil {
		return "warning", "模型列表", "当前 LLM 客户端不支持列模型。"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := lister.ListModels(ctx)
	if err != nil {
		return "error", "模型列表", "获取失败：" + err.Error()
	}
	if len(models) == 0 {
		return "info", "模型列表", "端点未返回任何模型。"
	}
	cur := h.activeModel(chatID)
	var sb strings.Builder
	sb.WriteString("可用模型：\n")
	for _, m := range models {
		mark := "  "
		if m == cur {
			mark = "→ "
		}
		sb.WriteString(mark + m + "\n")
	}
	sb.WriteString("\n/model <ID> 切换。")
	return "info", "模型列表", sb.String()
}

// cmdDirectory pins/clears/selects the per-chat working directory:
//
//	/cd            → interactive picker (scan WORKSPACE_ROOT subdirs)
//	/cd clear      → clear pin (fall back to global workspace_root)
//	/cd <path>     → pin directly (must be under WORKSPACE_ROOT)
func (h *Handler) cmdDirectory(chatID, arg string) (level, title, body string) {
	if h.workspaceRoot == "" {
		return "warning", "工作目录", "未配置 workspace_root，无法设置工作目录。"
	}
	root, err := filepath.Abs(h.workspaceRoot)
	if err != nil {
		return "error", "工作目录", "解析 workspace_root 失败：" + err.Error()
	}
	if arg == "" {
		// Interactive picker: scan WORKSPACE_ROOT for subdirectories.
		h.sendCtrl(&protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "info", Title: "正在扫描目录", Message: "正在获取工作目录列表，请稍候…"},
		})
		go func() {
			dirs := scanSubdirs(root)
			if len(dirs) == 0 {
				h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "warning", "工作目录", "WORKSPACE_ROOT 下没有子目录。")
				return
			}
			// Show basename in the card; resolve back to full path on click.
			names := make([]string, len(dirs))
			for i, d := range dirs {
				names[i] = filepath.Base(d)
			}
			choice, err := h.askAndWait(context.Background(), chatID, "目录", names)
			if err != nil {
				h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "warning", "选择失败", err.Error())
				return
			}
			// Resolve basename → full path.
			dir := ""
			for _, d := range dirs {
				if filepath.Base(d) == choice {
					dir = d
					break
				}
			}
			if dir == "" {
				h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "error", "工作目录", "选中的目录不存在。")
				return
			}
			if err := h.history.SetDir(chatID, dir); err != nil {
				h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "error", "工作目录", "设置失败："+err.Error())
				return
			}
			h.notifyWithPromptID(chatID, h.PromptIDForPickers(chatID), "success", "已切换目录", "工作目录已切换到 "+dir+"（下次提问生效）。")
		}()
		return "async", "", ""
	}
	if arg == "clear" {
		if err := h.history.SetDir(chatID, ""); err != nil {
			return "error", "工作目录", "清除失败：" + err.Error()
		}
		return "success", "已恢复默认", "已清除自定义工作目录，将使用全局 " + root + "。"
	}
	// /cd <path>: resolve and validate under WORKSPACE_ROOT.
	dir, err := resolveUnderRoot(root, arg)
	if err != nil {
		return "error", "工作目录", err.Error()
	}
	if err := h.history.SetDir(chatID, dir); err != nil {
		return "error", "工作目录", "设置失败：" + err.Error()
	}
	return "success", "已切换目录", "工作目录已切换到 " + dir + "（下次提问生效）。"
}

// scanSubdirs returns the absolute paths of immediate subdirectories under
// root, sorted by name. Used by the /cd picker.
func scanSubdirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
}
