package claudebridge

import (
	"context"

	"github.com/hu/lark-bridge/internal/bridgebase"
)

// cmdRunning lists all currently active turns across all chats. The snapshot
// and rendering live in bridgebase.Core; only the empty-state copy is
// backend-local.
func (h *Handler) cmdRunning(_ context.Context, _ string, _ []string) (commandResult, error) {
	sessions := h.RunningSessions()
	if len(sessions) == 0 {
		return commandResult{Body: "当前没有运行中的会话。"}, nil
	}
	return commandResult{Body: bridgebase.FormatRunning(sessions)}, nil
}
