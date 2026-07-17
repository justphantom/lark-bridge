package goosebridge

import (
	"context"
	"time"

	"github.com/hu/lark-bridge/internal/bridgebase"
)

// cmdRunning lists all currently active turns across all chats, so users can
// gauge system load and spot stuck or long-running sessions.
func (h *Handler) cmdRunning(_ context.Context, _ string, _ []string) (commandResult, error) {
	sessions := h.getRunningSessions()
	if len(sessions) == 0 {
		return commandResult{Body: "当前没有运行中的会话。"}, nil
	}
	return commandResult{Body: bridgebase.FormatRunning(sessions)}, nil
}

// getRunningSessions collects information about all currently active turns.
// It snapshots (chatID, *bridgebase.PromptCancel) under cancelMu, then releases the lock
// before querying the router — the router takes its own lock internally, so
// holding cancelMu across router.Lookup would risk an AB-BA deadlock against
// paths that take the locks in the opposite order.
func (h *Handler) getRunningSessions() []bridgebase.RunningSession {
	h.CancelMu.Lock()
	type entry struct {
		chatID string
		pc     *bridgebase.PromptCancel
	}
	entries := make([]entry, 0, len(h.CancelByChat))
	for chatID, pc := range h.CancelByChat {
		entries = append(entries, entry{chatID: chatID, pc: pc})
	}
	h.CancelMu.Unlock()

	now := time.Now()
	sessions := make([]bridgebase.RunningSession, 0, len(entries))
	for _, e := range entries {
		title := h.Router.TitleOf(e.chatID)
		if title == "" {
			title = "(未命名群组)"
		}
		binding, ok := h.Router.Lookup(e.chatID)
		model := "默认"
		if ok && binding.ModelSpec != "" {
			model = binding.ModelSpec
		}
		sessions = append(sessions, bridgebase.RunningSession{
			ChatID:    e.chatID,
			Title:     title,
			StartTime: e.pc.StartTime,
			Duration:  now.Sub(e.pc.StartTime),
			Model:     model,
		})
	}
	return sessions
}
