package peribridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/hu/lark-bridge/internal/log"
)

// cmdSessionNew resets the working context for the next prompt. peri is
// stateless (no session id), so "new conversation" means clearing the
// model pin — the directory is preserved so files on disk remain reachable.
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

// cmdCurrent shows the current binding's directory and model. peri has no
// session id, so that field is intentionally absent.
func (h *Handler) cmdCurrent(_ context.Context, chatID string, _ []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	var sb strings.Builder
	sb.WriteString("当前会话：\n")
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
	return commandResult{Body: sb.String()}, nil
}

// cmdHelp returns the auto-generated command list.
func (*Handler) cmdHelp(_ context.Context, _ string, _ []string) (commandResult, error) {
	return commandResult{Body: renderCmdHelp()}, nil
}
