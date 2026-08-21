package miniagent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cmdRunning lists currently active turns for this chat.
func (h *Handler) cmdRunning(_ context.Context, chatID, _ string) (level, title, body string) {
	sessions := h.RunningSessions()
	var filtered []RunningSession
	for _, s := range sessions {
		if s.ChatID == chatID {
			filtered = append(filtered, s)
		}
	}
	if len(filtered) == 0 {
		return "info", "运行中会话", "当前没有运行中的会话。"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "🔄 **运行中会话** (%d)\n\n", len(filtered))
	now := time.Now()
	for _, s := range filtered {
		fmt.Fprintf(&sb, "- 群ID：`%s`（运行 %s）\n", s.ChatID, FormatDuration(now.Sub(s.StartTime)))
	}
	sb.WriteString("\n💡 如需中止，请发送 `/abort`")
	return "info", "运行中会话", sb.String()
}
