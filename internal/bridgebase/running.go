package bridgebase

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

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
	sb.WriteString(fmt.Sprintf("🔄 **运行中会话** (%d)\n\n", len(sessions)))
	for i, session := range sessions {
		sb.WriteString(fmt.Sprintf("**%d. %s**\n", i+1, session.Title))
		sb.WriteString(fmt.Sprintf("   📊 群ID：`%s`\n", session.ChatID))
		sb.WriteString(fmt.Sprintf("   ⏱️ 运行时间：%s\n", FormatDuration(session.Duration)))
		sb.WriteString(fmt.Sprintf("   🤖 模型：%s\n", session.Model))
		if session.Agent != "" {
			sb.WriteString(fmt.Sprintf("   🔧 agent：%s\n", session.Agent))
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
