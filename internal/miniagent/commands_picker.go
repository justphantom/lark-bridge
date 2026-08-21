package miniagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/justphantom/lark-bridge/internal/miniclient"
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
		// Interactive picker: ListModels may take seconds and askAndWait
		// blocks for a human click; both must run off the SSE event loop.
		// Launch a goroutine that forks `miniagent -list-models`, emits a
		// picker Question morphing the progress card, waits for the click,
		// and patches that same card with the result.
		promptID := h.PromptIDForPickers(chatID)
		go func() { //nolint:gosec // G118: picker outlives the request ctx — the user's click may come minutes later
			pickCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			refs, err := h.client.ListModels(pickCtx, h.activeConfig(chatID))
			if err != nil {
				h.notifyWithPromptID(chatID, promptID, "error", "选择失败", err.Error())
				return
			}
			if len(refs) == 0 {
				h.notifyWithPromptID(chatID, promptID, "warning", "模型列表为空", "端点未返回任何模型；可用 /model <id> 手动指定。")
				return
			}
			// 展示与回放：-list-models 每行带各自 provider（2099241）。单
			// provider 只显 model id；多 provider 加 provider 前缀区分。选中
			// 后按 label 反查 ModelRef，把 (provider,model) 成对写回 binding
			// —— miniagent -provider/-model 须成对（02f8f81）。
			multi := distinctProviders(refs) > 1
			options := make([]string, len(refs))
			labelToRef := make(map[string]miniclient.ModelRef, len(refs))
			for i, m := range refs {
				label := m.Model
				if multi {
					label = m.Provider + "/" + m.Model
				}
				options[i] = label
				labelToRef[label] = m
			}
			choice, messageID, err := h.askAndWait(pickCtx, chatID, promptID, "模型", options)
			if err != nil {
				h.notifyWithPromptID(chatID, promptID, "warning", "选择失败", err.Error())
				return
			}
			ref, ok := labelToRef[choice]
			if !ok {
				h.notifyWithCardUpdate(chatID, messageID, "error", "选择失败", "选中的模型不在列表中。")
				return
			}
			h.ensureBinding(chatID)
			h.router.SetModelSpec(chatID, ref.Model)
			h.router.SetProvider(chatID, ref.Provider)
			h.notifyWithCardUpdate(chatID, messageID, "success", "已切换模型", "已切换到模型 "+choice+"（下次提问生效）。")
		}()
		return "async", "", "" // sentinel: handleSessionCommand must not notify
	}
	if arg == "clear" {
		h.ensureBinding(chatID)
		h.router.SetModelSpec(chatID, "")
		h.router.SetProvider(chatID, "")
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义模型，将使用全局默认 %s。", displayModel(h.cfgProvider, h.cfgModel))
	}
	// /model <id>: 手动指定仅 model id，无 provider。清空 binding 上的旧
	// provider（避免旧 provider 配新 model），activeTurnConfig 回落全局
	// cfgProvider；若全局也未配，buildArgs 不传 -provider/-model，改由
	// miniagent.json 的 defaults 生效。
	h.ensureBinding(chatID)
	h.router.SetModelSpec(chatID, arg)
	h.router.SetProvider(chatID, "")
	return "success", "已切换模型", fmt.Sprintf("已切换到模型 %s（下次提问生效）。", arg)
}

// distinctProviders counts unique provider names among refs. Drives whether the
// model picker shows a "provider/model" prefix (multi) or just the model id.
func distinctProviders(refs []miniclient.ModelRef) int {
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		seen[r.Provider] = struct{}{}
	}
	return len(seen)
}

// displayModel formats a provider/model pair: "provider/model" when provider is
// set, else the bare model id. Used by /current and the /model clear notice.
func displayModel(provider, model string) string {
	if provider != "" {
		return provider + "/" + model
	}
	return model
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
	cache := NewDirCache(h.workspaceRoot)
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
func applyDir(chatID, dir string, h *Handler, cache *DirCache) error {
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
