package claudebridge

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/router"
)

// cmdListSessions lists every chat→session binding the router holds. The
// Claude backend has no central session registry, so this is purely the
// local binding table.
func (h *Handler) cmdListSessions(_ context.Context, chatID string, _ []string) (commandResult, error) {
	body := formatBindings(h.router.AllBindings(), chatID)
	return commandResult{Body: body}, nil
}

// cmdSessionNew resets the bound session id so the next prompt starts a
// fresh Claude conversation. The working directory is preserved (files
// on disk remain); only the conversational context (the --resume id)
// is dropped. Any in-flight prompt is aborted first so the old session
// is not resumed mid-turn.
func (h *Handler) cmdSessionNew(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	h.abortChat(chatID)
	h.router.SetSessionID(chatID, "")
	h.logger.Info("session reset (new conversation)", log.FieldChatID, chatID)
	return commandResult{Body: "已重置会话上下文。工作目录保留，发送消息即开始新对话。"}, nil
}

// cmdSessionAbort cancels the in-flight Claude turn for this chat, if any.
func (h *Handler) cmdSessionAbort(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if h.abortChat(chatID) {
		return commandResult{Body: "已中止当前 Claude 调用。"}, nil
	}
	return commandResult{Body: "当前没有正在执行的 Claude 调用。"}, nil
}

// cmdSessionDel removes the binding entirely; the next prompt recreates
// a fresh binding (new directory + new session). Use /session-new to
// keep the directory but reset the conversation.
func (h *Handler) cmdSessionDel(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话绑定。"}, nil
	}
	h.abortChat(chatID)
	h.router.Unbind(chatID)
	h.logger.Info("binding deleted", log.FieldChatID, chatID)
	return commandResult{Body: "已删除会话绑定。下次提问将创建新会话与新目录。"}, nil
}

// cmdCurrent shows the current binding's directory, session id, and model.
// If the chat has no binding yet (no conversation started), one is created
// lazily so the command reflects the pre-prompt configuration.
func (h *Handler) cmdCurrent(_ context.Context, chatID string, _ []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	var sb strings.Builder
	sb.WriteString("当前会话：\n")
	if b.Title != "" {
		fmt.Fprintf(&sb, "  名称：%s\n", b.Title)
	}
	fmt.Fprintf(&sb, "  目录：%s\n", b.Directory)
	sid := b.SessionID
	if sid == "" {
		sid = "(未创建，首次提问后生成)"
	}
	fmt.Fprintf(&sb, "  会话ID：%s\n", sid)
	model := b.ModelSpec
	if model == "" {
		model = "(默认)"
	}
	fmt.Fprintf(&sb, "  模型：%s\n", model)
	perm := b.PermissionMode
	if perm == "" {
		perm = "默认 (" + h.permissionDefault + ")"
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

// formatBindings renders the binding map for /session-list, sorted by
// title for stable output. The current chat is marked with ←.
func formatBindings(bindings map[string]router.Binding, currentChat string) string {
	if len(bindings) == 0 {
		return "暂无会话绑定。"
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
