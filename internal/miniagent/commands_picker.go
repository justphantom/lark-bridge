package miniagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
)

// cmdModel pins or clears the per-chat model:
//
//	/model            → interactive picker (fetches /v1/models)
//	/model clear      → clear pin (fall back to global default)
//	/model <id>       → pin <id> for this chat
//
// The binding is created on demand because Router.SetModelSpec is a no-op
// on a missing binding.
//
// promptID comes from PromptIDForPickers (set by handleSessionCommand): the
// picker Question carries it + TakeOverProgress so the frontend morphs the
// command's progress card into the picker card; the result patches the same
// card via UpdateMessageID. A pre-answer failure terminates that card in
// place via notifyWithPromptID; a post-answer failure patches the picker card.
func (h *Handler) cmdModel(_ context.Context, chatID, arg string) (level, title, body string) {
	if arg == "" {
		// Interactive picker: fetchModels may take seconds and askAndWait
		// blocks for a human click; both must run off the SSE event loop.
		// Launch a goroutine that fetches models via HTTP, emits a picker
		// Question morphing the progress card, waits for the click, and
		// patches that same card with the result.
		promptID := h.PromptIDForPickers(chatID)
		go func() { //nolint:gosec // G118: picker outlives the request ctx — the user's click may come minutes later
			pickCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			models, err := fetchModels(pickCtx, h.client.BaseURL(), h.client.APIKey())
			if err != nil {
				h.notifyWithPromptID(chatID, promptID, "error", "选择失败", err.Error())
				return
			}
			if len(models) == 0 {
				h.notifyWithPromptID(chatID, promptID, "warning", "模型列表为空", "端点未返回任何模型；可用 /model <id> 手动指定。")
				return
			}
			choice, messageID, err := h.askAndWait(pickCtx, chatID, promptID, "模型", models)
			if err != nil {
				h.notifyWithPromptID(chatID, promptID, "warning", "选择失败", err.Error())
				return
			}
			h.ensureBinding(chatID)
			h.router.SetModelSpec(chatID, choice)
			h.notifyWithCardUpdate(chatID, messageID, "success", "已切换模型", "已切换到模型 "+choice+"（下次提问生效）。")
		}()
		return "async", "", "" // sentinel: handleSessionCommand must not notify
	}
	if arg == "clear" {
		h.ensureBinding(chatID)
		h.router.SetModelSpec(chatID, "")
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义模型，将使用全局默认 %s。", h.cfgModel)
	}
	h.ensureBinding(chatID)
	h.router.SetModelSpec(chatID, arg)
	return "success", "已切换模型", fmt.Sprintf("已切换到模型 %s（下次提问生效）。", arg)
}

// cmdModels lists available models from the OpenAI-compatible endpoint.
func (h *Handler) cmdModels(ctx context.Context, chatID, _ string) (level, title, body string) {
	models, err := fetchModels(ctx, h.client.BaseURL(), h.client.APIKey())
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
//	/cd            → interactive picker (WORKSPACE_ROOT subdirs)
//	/cd clear      → clear pin (fall back to global workspace_root)
//	/cd <path>     → pin directly (must be under WORKSPACE_ROOT)
//
// The chosen directory is MkdirAll'd with 0o700 (claude-back parity) so the
// first prompt after /cd does not fail on a non-existent workdir; miniagent
// spawns tool subprocesses (bash, git…) inside it.
//
// The picker reuses the command's progress card (see cmdModel): promptID +
// TakeOverProgress morph it into the picker, and the result patches the same
// card via UpdateMessageID.
func (h *Handler) cmdDirectory(_ context.Context, chatID, arg string) (level, title, body string) {
	cache := bridgebase.NewDirCache(h.workspaceRoot)
	if arg == "" {
		// Interactive picker: scan WORKSPACE_ROOT for subdirectories.
		promptID := h.PromptIDForPickers(chatID)
		go func() { //nolint:gosec // G118: picker outlives the request ctx
			dirs, err := cache.List()
			if err != nil {
				h.notifyWithPromptID(chatID, promptID, "warning", "工作目录", err.Error())
				return
			}
			if len(dirs) == 0 {
				h.notifyWithPromptID(chatID, promptID, "warning", "工作目录", "WORKSPACE_ROOT 下没有子目录。")
				return
			}
			// Show basename in the card; resolve back to full path on click.
			names := make([]string, len(dirs))
			byName := make(map[string]string, len(dirs))
			for i, d := range dirs {
				name := filepath.Base(d)
				names[i] = name
				byName[name] = d
			}
			choice, messageID, err := h.askAndWait(context.Background(), chatID, promptID, "目录", names)
			if err != nil {
				h.notifyWithPromptID(chatID, promptID, "warning", "选择失败", err.Error())
				return
			}
			dir, ok := byName[choice]
			if !ok {
				h.notifyWithCardUpdate(chatID, messageID, "error", "工作目录", "选中的目录不存在。")
				return
			}
			if err := applyDir(chatID, dir, h, cache); err != nil {
				h.notifyWithCardUpdate(chatID, messageID, "error", "工作目录", err.Error())
				return
			}
			h.notifyWithCardUpdate(chatID, messageID, "success", "已切换目录", "工作目录已切换到 "+dir+"（下次提问生效）。")
		}()
		return "async", "", ""
	}
	if arg == "clear" {
		h.ensureBinding(chatID)
		h.router.SetDirectory(chatID, "")
		return "success", "已恢复默认", "已清除自定义工作目录，将使用全局 " + h.workspaceRoot + "。"
	}
	// /cd <path>: validate under WORKSPACE_ROOT then create + bind.
	if err := applyDir(chatID, arg, h, cache); err != nil {
		return "error", "工作目录", err.Error()
	}
	return "success", "已切换目录", "工作目录已切换到 " + filepath.Clean(arg) + "（下次提问生效）。"
}

// applyDir validates dir under WORKSPACE_ROOT, MkdirAll's it with 0o700, and
// binds it on the chat. Returns the first failure. Split out so the picker
// and the /cd <path> path share identical side-effects.
func applyDir(chatID, dir string, h *Handler, cache *bridgebase.DirCache) error {
	cleaned := filepath.Clean(dir)
	if err := cache.Validate(cleaned); err != nil {
		return err
	}
	if err := os.MkdirAll(cleaned, 0o700); err != nil {
		return fmt.Errorf("创建目录失败：%w", err)
	}
	h.ensureBinding(chatID)
	h.router.SetDirectory(chatID, cleaned)
	return nil
}
