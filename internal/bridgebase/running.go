package bridgebase

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RunningSessions snapshots all in-flight prompt turns for the /running card.
// It copies (chatID, *PromptCancel) under CancelMu, then releases the lock
// before querying the router — the router takes its own lock internally, so
// holding CancelMu across router.Lookup would risk an AB-BA deadlock against
// paths that take the locks in the opposite order.
//
// Model/Agent come from the router binding; backends without an agent concept
// leave binding.Agent empty and RunningSession.Agent renders as nothing.
func (c *Core) RunningSessions() []RunningSession {
	c.CancelMu.Lock()
	type entry struct {
		chatID string
		pc     *PromptCancel
	}
	entries := make([]entry, 0, len(c.CancelByChat))
	for chatID, pc := range c.CancelByChat {
		entries = append(entries, entry{chatID: chatID, pc: pc})
	}
	c.CancelMu.Unlock()

	now := time.Now()
	sessions := make([]RunningSession, 0, len(entries))
	for _, e := range entries {
		title := c.Router.TitleOf(e.chatID)
		if title == "" {
			title = "(未命名群组)"
		}
		binding, ok := c.Router.Lookup(e.chatID)
		session := RunningSession{
			ChatID:    e.chatID,
			Title:     title,
			StartTime: e.pc.StartTime,
			Duration:  now.Sub(e.pc.StartTime),
			Model:     "默认",
		}
		if ok {
			if binding.ModelSpec != "" {
				session.Model = binding.ModelSpec
			}
			session.Agent = binding.Agent
		}
		sessions = append(sessions, session)
	}
	return sessions
}

// RunningSession describes one in-flight prompt turn for the /running card.
type RunningSession struct {
	ChatID    string
	Title     string
	StartTime time.Time
	Duration  time.Duration
	Model     string
	// Agent is rendered only when non-empty: backends with an agent concept
	// (opencode) set it, others leave "" and the line is omitted.
	Agent string
}

// FormatRunning renders the /running card body. sessions is sorted by start
// time (newest first) in place.
func FormatRunning(sessions []RunningSession) string {
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartTime.After(sessions[j].StartTime)
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "🔄 **运行中会话** (%d)\n\n", len(sessions))
	for i, session := range sessions {
		fmt.Fprintf(&sb, "**%d. %s**\n", i+1, session.Title)
		fmt.Fprintf(&sb, "   📊 群ID：`%s`\n", session.ChatID)
		fmt.Fprintf(&sb, "   ⏱️ 运行时间：%s\n", FormatDuration(session.Duration))
		fmt.Fprintf(&sb, "   🤖 模型：%s\n", session.Model)
		if session.Agent != "" {
			fmt.Fprintf(&sb, "   🔧 agent：%s\n", session.Agent)
		}
		sb.WriteString("\n")
	}
	sb.WriteString("💡 如需中止，请到对应群内发送 `/session-abort`")
	return sb.String()
}

// FormatDuration formats a duration with Chinese unit suffixes (秒/分/小时)
// for the /running card.
func FormatDuration(d time.Duration) string {
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
