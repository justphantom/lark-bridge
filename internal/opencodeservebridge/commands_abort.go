package opencodeservebridge

import (
	"context"
	"strings"
)

// cmdSessionAbort cancels the in-flight opencode turn for this chat AND
// POSTs /session/{id}/abort on the serve server. The latter is required even
// when the bridge believes no local turn is running: a prior prompt may have
// left the session in a server-side 'busy' state (e.g. the pump gave up but
// the server kept processing), which would block every subsequent message
// POST until the stuck turn finishes. Unlike CLI mode, the serve server
// owns the session lifecycle, so a server-side abort is the only way to
// release it.
func (h *Handler) cmdSessionAbort(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	local := h.AbortChat(chatID)

	binding, ok := h.Router.Lookup(chatID)
	if !ok || binding.SessionID == "" {
		if local {
			return commandResult{Body: "已中止当前 opencode 调用。"}, nil
		}
		return commandResult{Body: "当前群尚无会话绑定，无服务端 turn 可中止。"}, nil
	}

	serverErr := h.agent.AbortSession(ctx, binding.SessionID)
	var sb strings.Builder
	if local {
		sb.WriteString("已中止本地调用")
	} else {
		sb.WriteString("本地无进行中的调用")
	}
	if serverErr == nil {
		if sb.Len() > 0 {
			sb.WriteString("；")
		}
		sb.WriteString("已向 opencode serve 发送 abort（sessionID=" + binding.SessionID + "）")
	} else {
		if sb.Len() > 0 {
			sb.WriteString("；")
		}
		sb.WriteString("服务端 abort 失败：" + serverErr.Error())
	}
	return commandResult{Body: sb.String()}, nil
}
