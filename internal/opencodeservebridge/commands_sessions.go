package opencodeservebridge

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// cmdListSessions lists the sessions of the chat's working directory. It is
// read-only: it uses Lookup (not ensureBinding) so it never creates a
// binding as a side effect.
func (h *Handler) cmdListSessions(ctx context.Context, chatID string, _ []string) (commandResult, error) {
	b, ok := h.Router.Lookup(chatID)
	if !ok || b.Directory == "" {
		return commandResult{Body: "尚未设置工作目录。发送 `/cd` 选择一个项目目录后再查看会话。"}, nil
	}

	sessions, err := h.agent.ListSessions(ctx, b.Directory)
	if err != nil {
		return commandResult{Body: fmt.Sprintf("获取会话列表失败：%v", err)}, err
	}

	if len(sessions) == 0 {
		return commandResult{Body: "当前目录下没有任何会话。"}, nil
	}

	return commandResult{Body: formatSessions(sessions)}, nil
}

// formatSessions renders the session list for display.
func formatSessions(sessions []oc.SessionInfo) string {
	// Sort by updated time (most recent first)
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Time.Updated > sessions[j].Time.Updated
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 **所有会话** (%d)\n\n", len(sessions))

	for i, sess := range sessions {
		fmt.Fprintf(&sb, "**%d. %s**\n", i+1, sessTitle(sess.Title))
		fmt.Fprintf(&sb, "   🆔 会话ID：`%s`\n", sess.ID)

		if sess.Agent != "" {
			fmt.Fprintf(&sb, "   🔧 agent：%s\n", sess.Agent)
		}

		if sess.Model != nil {
			fmt.Fprintf(&sb, "   🤖 模型：%s\n", modelString(sess.Model))
		}

		if sess.Cost > 0 {
			fmt.Fprintf(&sb, "   💰 费用：$%.4f\n", sess.Cost)
		}

		if totalTokens := sess.Tokens.Input + sess.Tokens.Output + sess.Tokens.Reasoning; totalTokens > 0 {
			fmt.Fprintf(&sb, "   📊 Tokens：%.0f (入: %.0f, 出: %.0f", totalTokens, sess.Tokens.Input, sess.Tokens.Output)
			if sess.Tokens.Reasoning > 0 {
				fmt.Fprintf(&sb, ", 思维: %.0f", sess.Tokens.Reasoning)
			}
			if sess.Tokens.Cache.Read > 0 || sess.Tokens.Cache.Write > 0 {
				fmt.Fprintf(&sb, ", 缓存: %.0f读/%.0f写", sess.Tokens.Cache.Read, sess.Tokens.Cache.Write)
			}
			fmt.Fprintf(&sb, ")\n")
		}

		fmt.Fprintf(&sb, "   ⏱️ 更新：%s\n", formatTime(sess.Time.Updated))
		sb.WriteString("\n")
	}

	return sb.String()
}

// sessTitle returns a display title; defaults to "(未命名会话)" if empty.
func sessTitle(title string) string {
	if title == "" {
		return "(未命名会话)"
	}
	return title
}

// modelString formats a ModelRef as "provider/model".
func modelString(m *oc.ModelRef) string {
	if m == nil {
		return "(默认)"
	}
	if m.ProviderID == "" {
		return m.ID
	}
	return m.ProviderID + "/" + m.ID
}

// formatTime converts a millisecond timestamp to a relative time string.
func formatTime(ms int64) string {
	if ms == 0 {
		return "(未知)"
	}

	// Parse timestamp as local time (no timezone conversion)
	t := time.UnixMilli(ms)
	now := time.Now()
	diff := now.Sub(t)

	switch {
	case diff < time.Minute:
		return "刚刚"
	case diff < time.Hour:
		return fmt.Sprintf("%d分钟前", int(diff.Minutes()))
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1小时前"
		}
		return fmt.Sprintf("%d小时前", hours)
	case diff < 7*24*time.Hour:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1天前"
		}
		return fmt.Sprintf("%d天前", days)
	default:
		// For older sessions, show the date
		return t.Format("2006-01-02 15:04")
	}
}
