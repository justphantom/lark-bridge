package miniagent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// cmdPermission pins or clears the per-chat permission mode:
//
//	/perm              → show current + available modes
//	/perm plan         → read-only (read_file + webfetch only)
//	/perm default      → full tools with workspace bounds + blocklist
//	/perm free         → full tools without limits
//	/perm clear        → restore global default
func (h *Handler) cmdPermission(ctx context.Context, chatID, arg string) (level, title, body string) {
	if h.cli == nil {
		return "warning", "权限", "状态未启用，无法设置权限模式。"
	}
	if arg == "" {
		cur := h.activePermission(ctx, chatID)
		return "info", "当前权限", fmt.Sprintf("当前权限模式：%s\n\n可选：\n/perm plan（只读）\n/perm default（受限写）\n/perm free（无限制）\n/perm clear（恢复默认）", cur)
	}
	valid := map[string]bool{"plan": true, "default": true, "free": true, "clear": true}
	if !valid[arg] {
		return "warning", "权限", fmt.Sprintf("未知模式 %q。可选：plan / default / free / clear", arg)
	}
	mode := arg
	if arg == "clear" {
		mode = ""
	}
	if err := h.cli.SetPermission(ctx, chatID, mode); err != nil {
		return "error", "权限", "设置失败：" + err.Error()
	}
	if mode == "" {
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义权限，将使用全局默认 %s。", h.cfgPermission)
	}
	return "success", "已切换权限", fmt.Sprintf("已切换到权限模式 %s（下次提问生效）。", mode)
}

// cmdHelp lists all available commands with brief descriptions.
func (h *Handler) cmdHelp(_ context.Context, _ string, _ string) (level, title, body string) {
	var sb strings.Builder
	sb.WriteString("可用命令：\n\n")
	sb.WriteString("/current        显示当前会话/模型/目录/权限\n")
	sb.WriteString("/model          切换模型（弹出选择卡）\n")
	sb.WriteString("/model <id>     直接指定模型\n")
	sb.WriteString("/model clear    恢复默认模型\n")
	sb.WriteString("/models         列出可用模型\n")
	sb.WriteString("/cd             切换工作目录（弹出选择卡）\n")
	sb.WriteString("/cd <path>     直接指定目录\n")
	sb.WriteString("/cd clear       恢复默认目录\n")
	sb.WriteString("/perm           显示当前权限模式\n")
	sb.WriteString("/perm plan     只读（不能写文件/执行命令）\n")
	sb.WriteString("/perm default   受限写（路径限制+黑名单）\n")
	sb.WriteString("/perm free      无限制\n")
	sb.WriteString("/perm clear     恢复默认权限\n")
	sb.WriteString("/session-new    开启新会话\n")
	sb.WriteString("/session-list   列出所有会话\n")
	sb.WriteString("/session-use <id> 切换到指定会话\n")
	sb.WriteString("/session-del [id] 删除会话\n")
	sb.WriteString("/session-abort  中止当前任务\n")
	sb.WriteString("/memory-list [prefix] 列出长期记忆\n")
	sb.WriteString("/memory-del <key>  删除长期记忆\n")
	sb.WriteString("/memory-search <q> 搜索长期记忆\n")
	sb.WriteString("/running        显示运行中的会话\n")
	sb.WriteString("/help           显示本帮助\n")
	sb.WriteString("\n直接发送消息即可与 AI 对话。")
	return "info", "帮助", sb.String()
}

// cmdMemoryList lists long-term facts for the chat.
func (h *Handler) cmdMemoryList(ctx context.Context, chatID, arg string) (level, title, body string) {
	if h.cli == nil {
		return "warning", "长期记忆", "状态未启用。"
	}
	facts, err := h.cli.ListFacts(ctx, chatID, arg)
	if err != nil {
		return "error", "长期记忆", "读取失败：" + err.Error()
	}
	if len(facts) == 0 {
		if arg == "" {
			return "info", "长期记忆", "当前没有长期记忆。"
		}
		return "info", "长期记忆", fmt.Sprintf("前缀 %q 没有匹配的记忆。", arg)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "共 %d 条记忆：\n", len(facts))
	for _, f := range facts {
		fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
	}
	return "info", "长期记忆", sb.String()
}

// cmdMemoryDel deletes one long-term fact by key.
func (h *Handler) cmdMemoryDel(ctx context.Context, chatID, arg string) (level, title, body string) {
	if h.cli == nil {
		return "warning", "长期记忆", "状态未启用。"
	}
	if arg == "" {
		return "warning", "长期记忆", "用法：/memory-del <key>"
	}
	existed, err := h.cli.DeleteFact(ctx, chatID, arg)
	if err != nil {
		return "error", "长期记忆", "删除失败：" + err.Error()
	}
	if !existed {
		return "info", "长期记忆", fmt.Sprintf("记忆 %q 不存在。", arg)
	}
	return "success", "已删除", fmt.Sprintf("已删除记忆 %q。", arg)
}

// cmdMemorySearch searches long-term facts by substring.
func (h *Handler) cmdMemorySearch(ctx context.Context, chatID, arg string) (level, title, body string) {
	if h.cli == nil {
		return "warning", "长期记忆", "状态未启用。"
	}
	if arg == "" {
		return "warning", "长期记忆", "用法：/memory-search <关键词>"
	}
	facts, err := h.cli.SearchFacts(ctx, chatID, arg)
	if err != nil {
		return "error", "长期记忆", "搜索失败：" + err.Error()
	}
	if len(facts) == 0 {
		return "info", "长期记忆", fmt.Sprintf("未找到包含 %q 的记忆。", arg)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "找到 %d 条记忆：\n", len(facts))
	for _, f := range facts {
		fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
	}
	return "info", "长期记忆", sb.String()
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
