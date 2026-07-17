package miniagent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/protocol"
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

// sessionCmds is the single source of truth for command names → handlers.
// Adding a command means adding one entry here (and the method); isSession
// recognition and dispatch both read this table.
var sessionCmds = map[string]func(h *Handler, chatID, arg string) (level, title, body string){
	"/session-new":  (*Handler).cmdSessionNew,
	"/session-list": (*Handler).cmdSessionList,
	"/session-use":  (*Handler).cmdSessionUse,
	"/session-del":  (*Handler).cmdSessionDel,
	"/current":      (*Handler).cmdCurrent,
	"/model":        (*Handler).cmdModel,
	"/models":       (*Handler).cmdModels,
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

	fields := strings.Fields(prompt)
	arg := ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	fn := sessionCmds[fields[0]]
	level, title, body := fn(h, chatID, arg)
	// "async" is a sentinel: the command launched its own goroutine and
	// emitted its own notices, so handleSessionCommand must not send another.
	if level == "async" {
		return nil
	}
	return h.notify(ctx, chatID, level, title, body)
}

// Per-command handlers. Each returns the Notice level/title/body. history is
// non-nil (guarded by handleSessionCommand).

func (h *Handler) cmdSessionNew(chatID, _ string) (level, title, body string) {
	sid, err := h.history.NewSession(chatID)
	if err != nil {
		return "error", "会话", "新建会话失败：" + err.Error()
	}
	return "success", "已开启新会话",
		fmt.Sprintf("新会话已创建（%s）。旧会话仍保留，可用 /session-list 查看、/session-use 切回。", sid)
}

func (h *Handler) cmdSessionList(chatID, _ string) (level, title, body string) {
	return h.renderSessionList(chatID)
}

func (h *Handler) cmdSessionUse(chatID, arg string) (level, title, body string) {
	if arg == "" {
		return "warning", "会话", "用法：/session-use <会话ID>。先用 /session-list 查看可用会话。"
	}
	if err := h.history.UseSession(chatID, arg); err != nil {
		return "error", "会话", err.Error()
	}
	return "success", "已切换会话", fmt.Sprintf("已切换到会话 %s。", arg)
}

func (h *Handler) cmdSessionDel(chatID, arg string) (level, title, body string) {
	if err := h.history.DeleteSession(chatID, arg); err != nil {
		return "error", "会话", err.Error()
	}
	which := arg
	if which == "" {
		which = "当前活动会话"
	}
	return "success", "已删除", fmt.Sprintf("已删除会话 %s。", which)
}

func (h *Handler) cmdCurrent(chatID, _ string) (level, title, body string) {
	sid := h.history.Current(chatID)
	cur := h.activeModel(chatID)
	if sid == "" {
		return "info", "当前会话", fmt.Sprintf("当前无活动会话（首次提问后将自动创建）。\n模型：%s", cur)
	}
	return "info", "当前会话", fmt.Sprintf("活动会话：%s\n模型：%s", sid, cur)
}

// cmdModel pins or clears the per-chat model:
//   /model            → show current + usage
//   /model clear      → clear pin (fall back to global default)
//   /model <id>       → pin <id> for this chat
func (h *Handler) cmdModel(chatID, arg string) (level, title, body string) {
	if h.history == nil {
		return "warning", "模型", "记忆未启用，无法设置模型。"
	}
	if arg == "" {
		// No arg → interactive picker (async, like opencode-back's runModelPicker).
		// ListModels may take tens of seconds, and askAndWait blocks for a human
		// click; both must run off the SSE event loop. Emit a "loading" notice
		// immediately, then launch a goroutine that fetches models, emits a
		// selection card, waits for the user's click, and applies it.
		lister, ok := h.llm.(ModelLister)
		if !ok {
			return "warning", "模型", "当前 LLM 客户端不支持列模型。用 /model <ID> 直接指定。"
		}
		h.sendCtrl(&protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "info", Title: "正在加载模型列表", Message: "正在获取可用模型，请稍候…"},
		})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			models, err := lister.ListModels(ctx)
			if err != nil {
				h.notify(ctx, chatID, "error", "选择失败", "获取模型列表失败："+err.Error())
				return
			}
			choice, err := h.askAndWait(ctx, chatID, "模型", models)
			if err != nil {
				h.notify(ctx, chatID, "warning", "选择失败", err.Error())
				return
			}
			if err := h.history.SetModel(chatID, choice); err != nil {
				h.notify(ctx, chatID, "error", "模型", "设置失败："+err.Error())
				return
			}
			h.notify(ctx, chatID, "success", "已切换模型", "已切换到模型 "+choice+"（下次提问生效）。")
		}()
		return "async", "", "" // sentinel: handleSessionCommand must not notify
	}
	if arg == "clear" {
		if err := h.history.SetModel(chatID, ""); err != nil {
			return "error", "模型", "清除模型失败：" + err.Error()
		}
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义模型，将使用全局默认 %s。", h.cfg.Model)
	}
	if err := h.history.SetModel(chatID, arg); err != nil {
		return "error", "模型", "设置模型失败：" + err.Error()
	}
	return "success", "已切换模型", fmt.Sprintf("已切换到模型 %s（下次提问生效）。", arg)
}

// cmdModels lists available models from the LLM endpoint (GET /v1/models).
func (h *Handler) cmdModels(chatID, _ string) (level, title, body string) {
	lister, ok := h.llm.(ModelLister)
	if !ok {
		return "warning", "模型列表", "当前 LLM 客户端不支持列模型。"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := lister.ListModels(ctx)
	if err != nil {
		return "error", "模型列表", "获取失败：" + err.Error()
	}
	if len(models) == 0 {
		return "info", "模型列表", "端点未返回任何模型。"
	}
	cur := h.activeModel(chatID)
	var sb strings.Builder
	sb.WriteString("可用模型：\n")
	for _, m := range models {
		mark := "  "
		if m == cur {
			mark = "→ "
		}
		sb.WriteString(mark + m + "\n")
	}
	sb.WriteString("\n/model <ID> 切换。")
	return "info", "模型列表", sb.String()
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
