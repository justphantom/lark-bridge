package miniagent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cmdHelp lists the surviving commands after the stateless migration.
// Sessions / memory / perm / old -chat-id-style /cd are gone; the only
// persistent per-chat state is model + directory.
func (h *Handler) cmdHelp(_ context.Context, _ string, _ string) (level, title, body string) {
	var sb strings.Builder
	sb.WriteString("可用命令：\n\n")
	sb.WriteString("/current        显示当前模型/工作目录\n")
	sb.WriteString("/model          切换模型（弹出选择卡）\n")
	sb.WriteString("/model <id>     直接指定模型\n")
	sb.WriteString("/model clear    恢复默认模型\n")
	sb.WriteString("/models         列出可用模型\n")
	sb.WriteString("/cd             切换工作目录（弹出选择卡）\n")
	sb.WriteString("/cd <path>     直接指定目录\n")
	sb.WriteString("/cd clear       恢复默认目录\n")
	sb.WriteString("/pull           在当前工作目录执行 git pull --ff-only\n")
	sb.WriteString("/push           在当前工作目录执行 git push\n")
	sb.WriteString("/session-abort  中止当前任务\n")
	sb.WriteString("/running        显示运行中的会话\n")
	sb.WriteString("/help           显示本帮助\n")
	sb.WriteString("\n直接发送消息即可与 AI 对话。")
	return "info", "帮助", sb.String()
}

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
	for _, s := range filtered {
		fmt.Fprintf(&sb, "- 群ID：`%s`（运行 %s）\n", s.ChatID, formatDuration(s.Duration))
	}
	sb.WriteString("\n💡 如需中止，请发送 `/session-abort`")
	return "info", "运行中会话", sb.String()
}

// formatDuration formats elapsed time with Chinese suffixes.
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
