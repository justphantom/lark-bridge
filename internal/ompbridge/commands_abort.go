package ompbridge

import "context"

// cmdSessionAbort cancels the in-flight omp turn for this chat, if any.
// In CLI subprocess mode the only abort path is cancelling the local
// subprocess context (which SIGKILLs the omp process group); there is no
// server-side abort endpoint to call.
func (h *Handler) cmdSessionAbort(_ context.Context, chatID string, _ []string) (commandResult, error) {
	if h.AbortChat(chatID) {
		return commandResult{Body: "已中止当前 omp 调用。"}, nil
	}
	return commandResult{Body: "当前没有正在执行的 omp 调用。"}, nil
}
