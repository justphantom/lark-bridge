package claudebridge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// runningSession represents a currently active Claude session.
type runningSession struct {
	ChatID    string
	Title     string
	StartTime time.Time
	Duration  time.Duration
	Model     string
}

// cmdRunning lists all currently active Claude sessions across all chats.
// This helps users understand system load and identify stuck or long-running sessions.
func (h *Handler) cmdRunning(_ context.Context, _ string, _ []string) (commandResult, error) {
	sessions := h.getRunningSessions()

	if len(sessions) == 0 {
		return commandResult{Body: "当前没有运行中的会话。"}, nil
	}

	return commandResult{Body: h.formatRunningSessions(sessions)}, nil
}

// getRunningSessions collects information about all currently active
// sessions. It snapshots (chatID, *promptCancel) under cancelMu, then
// releases the lock before querying the router — the router takes its
// own lock internally, so holding cancelMu across router.Lookup would
// risk an AB-BA deadlock against paths that take the locks in the
// opposite order.
func (h *Handler) getRunningSessions() []runningSession {
	h.cancelMu.Lock()
	type entry struct {
		chatID string
		pc     *promptCancel
	}
	entries := make([]entry, 0, len(h.cancelByChat))
	for chatID, pc := range h.cancelByChat {
		entries = append(entries, entry{chatID: chatID, pc: pc})
	}
	h.cancelMu.Unlock()

	now := time.Now()
	sessions := make([]runningSession, 0, len(entries))
	for _, e := range entries {
		title := h.router.TitleOf(e.chatID)
		if title == "" {
			title = "(未命名群组)"
		}

		binding, ok := h.router.Lookup(e.chatID)
		model := "默认"
		if ok && binding.ModelSpec != "" {
			model = binding.ModelSpec
		}

		duration := now.Sub(e.pc.startTime)
		sessions = append(sessions, runningSession{
			ChatID:    e.chatID,
			Title:     title,
			StartTime: e.pc.startTime,
			Duration:  duration,
			Model:     model,
		})
	}

	// Sort by start time (newest first).
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})

	return sessions
}

// formatRunningSessions renders the running sessions list as a formatted string.
func (h *Handler) formatRunningSessions(sessions []runningSession) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("🔄 **运行中会话** (%d)\n\n", len(sessions)))

	for i, session := range sessions {
		sb.WriteString(fmt.Sprintf("**%d. %s**\n", i+1, session.Title))
		sb.WriteString(fmt.Sprintf("   📊 群ID：`%s`\n", session.ChatID))
		sb.WriteString(fmt.Sprintf("   ⏱️ 运行时间：%s\n", formatDuration(session.Duration)))
		sb.WriteString(fmt.Sprintf("   🤖 模型：%s\n", session.Model))
		sb.WriteString("\n")
	}

	sb.WriteString("💡 如需中止，请到对应群内发送 `/session-abort`")
	return sb.String()
}

// formatDuration formats a duration with Chinese unit suffixes (秒/分/小时)
// for the /running card.
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d秒", int(d.Seconds()))
	case d < time.Hour:
		minutes := int(d.Minutes())
		seconds := int(d.Seconds()) % 60
		if seconds > 0 {
			return fmt.Sprintf("%d分%d秒", minutes, seconds)
		}
		return fmt.Sprintf("%d分钟", minutes)
	default:
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		if minutes > 0 {
			return fmt.Sprintf("%d小时%d分", hours, minutes)
		}
		return fmt.Sprintf("%d小时", hours)
	}
}
