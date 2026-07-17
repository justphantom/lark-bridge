package opencodebridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/hu/lark-bridge/internal/log"
)

// cmdSessionNew resets the bound session id so the next prompt starts a fresh
// opencode conversation. The working directory is preserved (files on disk
// remain); only the conversational context (the --session id) is dropped. Any
// in-flight prompt is aborted first so the old session is not resumed
// mid-turn.
func (h *Handler) cmdSessionNew(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.Router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话，直接发送消息即可开始。"}, nil
	}
	h.abortChat(chatID)
	h.Router.SetSessionID(chatID, "")
	h.Logger.Info("session reset (new conversation)", log.FieldChatID, chatID)
	return commandResult{Body: "已重置会话上下文。工作目录保留，发送消息即开始新对话。"}, nil
}

// cmdSessionDel removes the binding entirely; the next prompt recreates a
// fresh binding (new directory + new session). Use /session-new to keep the
// directory but reset the conversation.
func (h *Handler) cmdSessionDel(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if _, ok := h.Router.Lookup(chatID); !ok {
		return commandResult{Body: "当前群尚无会话绑定。"}, nil
	}
	h.abortChat(chatID)
	h.Router.Unbind(chatID)
	h.Logger.Info("binding deleted", log.FieldChatID, chatID)
	return commandResult{Body: "已删除会话绑定。下次提问将创建新会话与新目录。"}, nil
}

// cmdCurrent shows the current binding's directory, session id, model and
// agent. If the chat has no binding yet (no conversation started), one is
// created lazily so the command reflects the pre-prompt configuration.
func (h *Handler) cmdCurrent(_ context.Context, chatID string, _ []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	var sb strings.Builder
	sb.WriteString("当前会话：\n")
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
	agent := b.Agent
	if agent == "" {
		agent = "(默认)"
	}
	fmt.Fprintf(&sb, "  Agent：%s\n", agent)
	return commandResult{Body: sb.String()}, nil
}

// cmdHelp returns the auto-generated command list.
func (*Handler) cmdHelp(_ context.Context, _ string, _ []string) (commandResult, error) {
	return commandResult{Body: renderCmdHelp()}, nil
}
