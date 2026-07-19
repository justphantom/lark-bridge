package miniagent

import (
	"context"
	"fmt"
	"strings"
)

// Session management commands. These mirror the claude backend's /session-*
// vocabulary so users get a consistent UX across bridges, but the dispatch is
// self-contained (the miniagent has no router/registry like claudebridge).
//
// State changes are issued through the CLI binary (cli_state.go), so the
// bridge never touches the on-disk session/jsonl/pin files directly.
// Each command returns the Notice level/title/body the dispatcher emits.
// "async" as level is a sentinel meaning the command has already emitted its
// own controls and the dispatcher must not emit a Notice.

// sessionCmds is the single source of truth for command names → handlers.
// Adding a command means adding one entry here (and the method); isSession
// recognition and dispatch both read this table.
var sessionCmds = map[string]func(h *Handler, ctx context.Context, chatID, arg string) (level, title, body string){
	"/session-new":   (*Handler).cmdSessionNew,
	"/session-list":  (*Handler).cmdSessionList,
	"/session-use":   (*Handler).cmdSessionUse,
	"/session-del":   (*Handler).cmdSessionDel,
	"/current":       (*Handler).cmdCurrent,
	"/model":         (*Handler).cmdModel,
	"/models":        (*Handler).cmdModels,
	"/cd":            (*Handler).cmdDirectory,
	"/perm":          (*Handler).cmdPermission,
	"/memory-list":   (*Handler).cmdMemoryList,
	"/memory-del":    (*Handler).cmdMemoryDel,
	"/memory-search": (*Handler).cmdMemorySearch,
	"/help":          (*Handler).cmdHelp,
}

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
	_, ok := sessionCmds[fields[0]]
	return ok
}

// handleSessionCommand reserves the per-chat turn slot (so a command cannot
// race with an in-flight runTurn over CLI state), runs the command, and
// replies via a Notice. A busy chat gets the same "处理中" notice a prompt
// would — the slot also blocks a session switch from landing mid-turn, which
// would orphan the in-flight turn's Append target.
func (h *Handler) handleSessionCommand(ctx context.Context, chatID, promptID, prompt string) error {
	if h.cli == nil && !isMemoryCommand(prompt) {
		h.notifyWithPromptID(chatID, promptID, "warning", "会话", "记忆未启用，无法管理会话。")
		return nil
	}
	turnCtx, mine, ok := h.startTurn(ctx, chatID)
	_ = turnCtx
	if !ok {
		h.notifyWithPromptID(chatID, promptID, "warning", "处理中", "上一条消息还在处理，请等它结束后再发。")
		return nil
	}
	defer h.endTurn(chatID, mine)
	h.SetPromptIDForPickers(chatID, promptID)
	defer h.SetPromptIDForPickers(chatID, "")

	fields := strings.Fields(prompt)
	arg := ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	fn := sessionCmds[fields[0]]
	level, title, body := fn(h, ctx, chatID, arg)
	if level == "async" {
		return nil
	}
	h.notifyWithPromptID(chatID, promptID, level, title, body)
	return nil
}

// isMemoryCommand reports whether prompt is one of the memory-management
// commands that should work even when state access is disabled.
func isMemoryCommand(prompt string) bool {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "/memory-list", "/memory-del", "/memory-search":
		return true
	}
	return false
}

// Per-command handlers. Each returns the Notice level/title/body. cli is
// non-nil (guarded by handleSessionCommand).

func (h *Handler) cmdSessionNew(ctx context.Context, chatID, _ string) (level, title, body string) {
	sid, err := h.cli.NewSession(ctx, chatID)
	if err != nil {
		return "error", "会话", "新建会话失败：" + err.Error()
	}
	return "success", "已开启新会话",
		fmt.Sprintf("新会话已创建（%s）。旧会话仍保留，可用 /session-list 查看、/session-use 切回。", sid)
}

func (h *Handler) cmdSessionList(ctx context.Context, chatID, _ string) (level, title, body string) {
	return h.renderSessionList(ctx, chatID)
}

func (h *Handler) cmdSessionUse(ctx context.Context, chatID, arg string) (level, title, body string) {
	if arg == "" {
		return "warning", "会话", "用法：/session-use <会话ID>。先用 /session-list 查看可用会话。"
	}
	if err := h.cli.UseSession(ctx, chatID, arg); err != nil {
		return "error", "会话", err.Error()
	}
	return "success", "已切换会话", fmt.Sprintf("已切换到会话 %s。", arg)
}

func (h *Handler) cmdSessionDel(ctx context.Context, chatID, arg string) (level, title, body string) {
	if err := h.cli.DeleteSession(ctx, chatID, arg); err != nil {
		return "error", "会话", err.Error()
	}
	which := arg
	if which == "" {
		which = "当前活动会话"
	}
	return "success", "已删除", fmt.Sprintf("已删除会话 %s。", which)
}

func (h *Handler) cmdCurrent(ctx context.Context, chatID, _ string) (level, title, body string) {
	state, err := h.cli.ShowCurrent(ctx, chatID)
	if err != nil {
		return "error", "当前状态", "读取失败：" + err.Error()
	}
	cur := state.Model
	if cur == "" {
		cur = h.cfgModel
	}
	dir := state.Directory
	if dir == "" {
		dir = h.workspaceRoot
	}
	perm := state.Permission
	if perm == "" {
		perm = h.cfgPermission
	}
	if state.SessionID == "" {
		return "info", "当前状态", fmt.Sprintf("当前无活动会话（首次提问后将自动创建）。\n模型：%s\n工作目录：%s\n权限：%s", cur, dir, perm)
	}
	return "info", "当前状态", fmt.Sprintf("活动会话：%s\n模型：%s\n工作目录：%s\n权限：%s", state.SessionID, cur, dir, perm)
}

// renderSessionList builds the /session-list body: oldest first, the active
// session marked with →, each line carrying id / mtime / size.
func (h *Handler) renderSessionList(ctx context.Context, chatID string) (level, title, body string) {
	sessions, err := h.cli.ListSessions(ctx, chatID)
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
