package opencodebridge

import "context"

// cmdSessionAbort cancels the in-flight opencode turn for this chat, if any.
// In CLI subprocess mode the only abort path is cancelling the local
// subprocess context (which SIGKILLs the opencode process); there is no
// server-side abort endpoint to call.
func (h *Handler) cmdSessionAbort(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if h.AbortChat(chatID) {
		return commandResult{Body: "已中止当前 opencode 调用。"}, nil
	}
	return commandResult{Body: "当前没有正在执行的 opencode 调用。"}, nil
}
