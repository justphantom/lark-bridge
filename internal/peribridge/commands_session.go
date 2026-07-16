package peribridge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/router"
)

// cmdListSessions lists every chat→binding the router holds. peri has no
// central session registry, so this is purely the local binding table (each
// row is a chat's working directory + pinned settings, not a live session).
func (h *Handler) cmdListSessions(_ context.Context, chatID string, _ []string) (commandResult, error) {
	body := formatBindings(h.router.AllBindings(), chatID)
	return commandResult{Body: body}, nil
}

// cmdSessionNew resets the working context for the next prompt. peri is
// stateless (no session id), so "new conversation" means clearing the model
// pin — the directory is preserved so files on disk remain reachable.
// Any in-flight prompt is aborted first.
func (h *Handler) cmdSessionNew(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无绑定，直接发送消息即可开始。"}, nil
	}
	h.abortChat(chatID)
	h.router.SetModelSpec(chatID, "")
	h.logger.Info("context reset (clear model pin)", log.FieldChatID, chatID)
	return commandResult{Body: "已重置工作上下文（模型设置已清除，目录保留）。发送消息即开始新对话。"}, nil
}

// cmdSessionDel removes the binding entirely; the next prompt recreates a
// fresh binding (new directory + default model).
func (h *Handler) cmdSessionDel(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无绑定。"}, nil
	}
	h.abortChat(chatID)
	h.router.Unbind(chatID)
	h.logger.Info("binding deleted", log.FieldChatID, chatID)
	return commandResult{Body: "已删除绑定。下次提问将创建新目录与默认设置。"}, nil
}

// cmdCurrent shows the current binding's directory, model, permission mode,
// effort level, and settings file. peri has no session id, so that field is
// intentionally absent.
func (h *Handler) cmdCurrent(_ context.Context, chatID string, _ []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	var sb strings.Builder
	sb.WriteString("当前会话：\n")
	if b.Title != "" {
		fmt.Fprintf(&sb, "  名称：%s\n", b.Title)
	}
	dir := b.Directory
	if dir == "" {
		dir = "(未设置，请用 /cd 选择)"
	}
	fmt.Fprintf(&sb, "  目录：%s\n", dir)
	model := b.ModelSpec
	if model == "" {
		model = "(默认，由 ~/.peri/settings.json 决定)"
	}
	fmt.Fprintf(&sb, "  模型：%s\n", model)
	perm := b.PermissionMode
	if perm == "" {
		perm = "默认 (bypass)"
	}
	fmt.Fprintf(&sb, "  权限模式：%s\n", perm)
	effort := b.EffortLevel
	if effort == "" {
		effort = "(默认)"
	}
	fmt.Fprintf(&sb, "  推理级别：%s\n", effort)
	settingsFile := b.SettingsFile
	if settingsFile == "" {
		settingsFile = "(未设置)"
	}
	fmt.Fprintf(&sb, "  settings文件：%s\n", settingsFile)
	return commandResult{Body: sb.String()}, nil
}

// cmdHelp returns the auto-generated command list.
func (*Handler) cmdHelp(_ context.Context, _ string, _ []string) (commandResult, error) {
	return commandResult{Body: renderCmdHelp()}, nil
}

// formatBindings renders the binding map for /session-list, sorted by title.
// The current chat is marked with ←.
func formatBindings(bindings map[string]router.Binding, currentChat string) string {
	if len(bindings) == 0 {
		return "暂无绑定。"
	}
	keys := make([]string, 0, len(bindings))
	for k := range bindings {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bindings[keys[i]].Title < bindings[keys[j]].Title
	})
	var sb strings.Builder
	for _, k := range keys {
		b := bindings[k]
		title := b.Title
		if title == "" {
			title = "(未命名)"
		}
		marker := ""
		if k == currentChat {
			marker = " ← 当前"
		}
		model := b.ModelSpec
		if model == "" {
			model = "默认"
		}
		fmt.Fprintf(&sb, "• %s [%s]%s\n", title, model, marker)
	}
	return sb.String()
}
