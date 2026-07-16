package peribridge

import "context"

// cmdSessionAbort cancels the in-flight peri turn for this chat, if any. In
// CLI subprocess mode the only abort path is cancelling the local subprocess
// context (which SIGKILLs the peri process group).
func (h *Handler) cmdSessionAbort(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if h.abortChat(chatID) {
		return commandResult{Body: "已中止当前 peri 调用。"}, nil
	}
	return commandResult{Body: "当前没有正在执行的 peri 调用。"}, nil
}
