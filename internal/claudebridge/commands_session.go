package claudebridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/log"
)

// cmdListSessions lists every Claude session under the chat's bound directory
// (filesystem scan of ~/.claude/projects/<encoded-cwd>), not just the local
// binding table — so sessions created out-of-band (other chats, or directly
// via the CLI) surface too. The bound directory MUST be set via /cd; a chat
// with no binding or no directory pin has nothing to list. The scan is
// sub-ms local I/O, so it runs synchronously (no async banner). The currently
// bound session is marked ★ so the user sees which row /session-clean keeps.
func (h *Handler) cmdListSessions(_ context.Context, chatID string, _ []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	if b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 /cd 选择一个项目目录后再查看会话。"}, nil
	}
	sessions, err := h.agent.ListSessions(h.AppCtx, b.Directory)
	if err != nil {
		return commandResult{Body: "获取会话列表失败：" + err.Error()}, nil
	}
	return commandResult{Body: formatSessionList(sessions, b.SessionID)}, nil
}

// cmdSessionNew resets the bound session id so the next prompt starts a
// fresh Claude conversation. The working directory is preserved (files
// on disk remain); only the conversational context (the --resume id)
// is dropped. Any in-flight prompt is aborted first so the old session
// is not resumed mid-turn.
func (h *Handler) cmdSessionNew(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.Router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	h.AbortChat(chatID)
	h.Router.SetSessionID(chatID, "")
	h.Logger.Info("session reset (new conversation)", log.FieldChatID, chatID)
	return commandResult{Body: "已重置会话上下文。工作目录保留，发送消息即开始新对话。"}, nil
}

// cmdSessionAbort cancels the in-flight Claude turn for this chat, if any.
func (h *Handler) cmdSessionAbort(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if h.AbortChat(chatID) {
		return commandResult{Body: "已中止当前 Claude 调用。"}, nil
	}
	return commandResult{Body: "当前没有正在执行的 Claude 调用。"}, nil
}

// cmdSessionDel removes the binding entirely; the next prompt recreates
// a fresh binding (new directory + new session). Use /session-new to
// keep the directory but reset the conversation.
func (h *Handler) cmdSessionDel(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.Router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话绑定。"}, nil
	}
	h.AbortChat(chatID)
	h.Router.Unbind(chatID)
	h.Logger.Info("binding deleted", log.FieldChatID, chatID)
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
		perm = "默认 (" + h.PermissionDefault + ")"
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
