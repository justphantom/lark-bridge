package miniagent

import (
	"context"
	"fmt"
	"strings"
)

// Session management commands. These mirror the claude backend's /session-*
// vocabulary so users get a consistent UX across bridges, but the dispatch is
// self-contained (the miniagent has no router/registry like claudebridge, so
// it does not use bridgebase.Commands). A command is recognized only when the
// prompt's first token is one of:
//
//	/session-new        start a fresh session (the old one is kept on disk)
//	/session-list       list this chat's stored sessions
//	/session-use <id>   resume a previously stored session (接续)
//	/session-del [<id>] delete a session (the active one when no id is given)
//	/current            show the active session id
//
// Commands are dispatched inline from HandleEvent (before the LLM turn) and
// the reply is emitted as a single TypeNotice.

// isSessionCommand reports whether prompt is one this handler owns. It never
// panics on a bare "/" — strings.Fields collapses that to nothing.
func isSessionCommand(prompt string) bool {
	if !strings.HasPrefix(prompt, "/") {
		return false
	}
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "/session-new", "/session-list", "/session-use", "/session-del", "/current":
		return true
	}
	return false
}

// handleSessionCommand reserves the per-chat turn slot (so it cannot race with
// an in-flight runTurn over the history files), runs the command, and replies
// via a Notice. A busy chat gets the same "处理中" notice a prompt would — the
// slot also blocks a session switch from landing mid-turn, which would orphan
// the in-flight turn's Append target.
func (h *Handler) handleSessionCommand(ctx context.Context, chatID, prompt string) error {
	if h.history == nil {
		return h.notify(ctx, chatID, "warning", "会话", "记忆未启用，无法管理会话。")
	}
	// Reserve the slot so a concurrent runTurn for this chat cannot interleave
	// a Load/Append with our session mutation. Synchronous + fast (file ops),
	// so turnCtx is unused; the slot purely serializes.
	turnCtx, mine, ok := h.startTurn(ctx, chatID)
	_ = turnCtx
	if !ok {
		return h.notify(ctx, chatID, "warning", "处理中", "上一条消息还在处理，请等它结束后再发。")
	}
	defer h.endTurn(chatID, mine)

	level, title, body := h.dispatchSession(chatID, prompt)
	return h.notify(ctx, chatID, level, title, body)
}

// dispatchSession parses and runs one session command, returning the Notice
// level/title/body. history is non-nil (guarded by the caller).
func (h *Handler) dispatchSession(chatID, prompt string) (level, title, body string) {
	fields := strings.Fields(prompt)
	cmd := fields[0]
	arg := ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	switch cmd {
	case "/session-new":
		sid, err := h.history.NewSession(chatID)
		if err != nil {
			return "error", "会话", "新建会话失败：" + err.Error()
		}
		return "success", "已开启新会话",
			fmt.Sprintf("新会话已创建（%s）。旧会话仍保留，可用 /session-list 查看、/session-use 切回。", sid)

	case "/session-list":
		return h.renderSessionList(chatID)

	case "/session-use":
		if arg == "" {
			return "warning", "会话", "用法：/session-use <会话ID>。先用 /session-list 查看可用会话。"
		}
		if err := h.history.UseSession(chatID, arg); err != nil {
			return "error", "会话", err.Error()
		}
		return "success", "已切换会话", fmt.Sprintf("已切换到会话 %s。", arg)

	case "/session-del":
		if err := h.history.DeleteSession(chatID, arg); err != nil {
			return "error", "会话", err.Error()
		}
		which := arg
		if which == "" {
			which = "当前活动会话"
		}
		return "success", "已删除", fmt.Sprintf("已删除会话 %s。", which)

	case "/current":
		sid := h.history.Current(chatID)
		if sid == "" {
			return "info", "当前会话", "当前无活动会话（首次提问后将自动创建）。"
		}
		return "info", "当前会话", fmt.Sprintf("活动会话：%s\n模型：%s", sid, h.cfg.Model)
	}
	return "warning", "会话", "未知命令：" + cmd
}

// renderSessionList builds the /session-list body: oldest first, the active
// session marked with →, each line carrying id / mtime / size.
func (h *Handler) renderSessionList(chatID string) (level, title, body string) {
	sessions, err := h.history.ListSessions(chatID)
	if err != nil {
		return "error", "会话", "读取会话列表失败：" + err.Error()
	}
	if len(sessions) == 0 {
		return "info", "会话", "暂无会话。发送消息即开始第一个会话。"
	}
	var sb strings.Builder
	sb.WriteString("会话列表：\n")
	for _, s := range sessions {
		mark := "  "
		if s.Current {
			mark = "→ "
		}
		fmt.Fprintf(&sb, "%s%s  (%s, %d B)\n", mark, s.ID, s.ModTime.Format("2006-01-02 15:04"), s.Bytes)
	}
	sb.WriteString("\n/session-use <ID> 切换，/session-del <ID> 删除。")
	return "info", "会话", sb.String()
}
