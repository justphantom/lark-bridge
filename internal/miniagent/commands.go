package miniagent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
	"/cd":           (*Handler).cmdDirectory,
	"/perm":         (*Handler).cmdPermission,
	"/help":         (*Handler).cmdHelp,
	"/running":      (*Handler).cmdRunning,
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
	dir := h.activeDir(chatID)
	perm := h.activePermission(chatID)
	if sid == "" {
		return "info", "当前状态", fmt.Sprintf("当前无活动会话（首次提问后将自动创建）。\n模型：%s\n工作目录：%s\n权限：%s", cur, dir, perm)
	}
	return "info", "当前状态", fmt.Sprintf("活动会话：%s\n模型：%s\n工作目录：%s\n权限：%s", sid, cur, dir, perm)
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
		lister := h.modelLister
		if lister == nil {
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
	lister := h.modelLister
	if lister == nil {
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

// cmdDirectory pins/clears/selects the per-chat working directory:
//   /cd            → interactive picker (scan WORKSPACE_ROOT subdirs)
//   /cd clear      → clear pin (fall back to global workspace_root)
//   /cd <path>     → pin directly (must be under WORKSPACE_ROOT)
func (h *Handler) cmdDirectory(chatID, arg string) (level, title, body string) {
	if h.workspaceRoot == "" {
		return "warning", "工作目录", "未配置 workspace_root，无法设置工作目录。"
	}
	root, err := filepath.Abs(h.workspaceRoot)
	if err != nil {
		return "error", "工作目录", "解析 workspace_root 失败：" + err.Error()
	}
	if arg == "" {
		// Interactive picker: scan WORKSPACE_ROOT for subdirectories.
		h.sendCtrl(&protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "info", Title: "正在扫描目录", Message: "正在获取工作目录列表，请稍候…"},
		})
		go func() {
			dirs := scanSubdirs(root)
			if len(dirs) == 0 {
				h.notify(context.Background(), chatID, "warning", "工作目录", "WORKSPACE_ROOT 下没有子目录。")
				return
			}
			// Show basename in the card; resolve back to full path on click.
			names := make([]string, len(dirs))
			for i, d := range dirs {
				names[i] = filepath.Base(d)
			}
			choice, err := h.askAndWait(context.Background(), chatID, "目录", names)
			if err != nil {
				h.notify(context.Background(), chatID, "warning", "选择失败", err.Error())
				return
			}
			// Resolve basename → full path.
			dir := ""
			for _, d := range dirs {
				if filepath.Base(d) == choice {
					dir = d
					break
				}
			}
			if dir == "" {
				h.notify(context.Background(), chatID, "error", "工作目录", "选中的目录不存在。")
				return
			}
			if err := h.history.SetDir(chatID, dir); err != nil {
				h.notify(context.Background(), chatID, "error", "工作目录", "设置失败："+err.Error())
				return
			}
			h.notify(context.Background(), chatID, "success", "已切换目录", "工作目录已切换到 "+dir+"（下次提问生效）。")
		}()
		return "async", "", ""
	}
	if arg == "clear" {
		if err := h.history.SetDir(chatID, ""); err != nil {
			return "error", "工作目录", "清除失败：" + err.Error()
		}
		return "success", "已恢复默认", "已清除自定义工作目录，将使用全局 " + root + "。"
	}
	// /cd <path>: resolve and validate under WORKSPACE_ROOT.
	dir, err := resolveUnderRoot(root, arg)
	if err != nil {
		return "error", "工作目录", err.Error()
	}
	if err := h.history.SetDir(chatID, dir); err != nil {
		return "error", "工作目录", "设置失败：" + err.Error()
	}
	return "success", "已切换目录", "工作目录已切换到 " + dir + "（下次提问生效）。"
}

// scanSubdirs returns the absolute paths of immediate subdirectories under
// root, sorted by name. Used by the /cd picker.
func scanSubdirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(root, e.Name()))
		}
	}
	return dirs
}

// cmdPermission pins or clears the per-chat permission mode:
//   /perm              → show current + available modes
//   /perm plan         → read-only (read_file + webfetch only)
//   /perm default      → full tools with workspace bounds + blocklist
//   /perm free         → full tools without limits
//   /perm clear        → restore global default
func (h *Handler) cmdPermission(chatID, arg string) (level, title, body string) {
	if h.history == nil {
		return "warning", "权限", "记忆未启用，无法设置权限模式。"
	}
	if arg == "" {
		cur := h.activePermission(chatID)
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
	if err := h.history.SetPermission(chatID, mode); err != nil {
		return "error", "权限", "设置失败：" + err.Error()
	}
	if mode == "" {
		return "success", "已恢复默认", fmt.Sprintf("已清除自定义权限，将使用全局默认 %s。", h.cfgPermission)
	}
	return "success", "已切换权限", fmt.Sprintf("已切换到权限模式 %s（下次提问生效）。", mode)
}

// cmdHelp lists all available commands with brief descriptions.
func (h *Handler) cmdHelp(_, _ string) (level, title, body string) {
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
	sb.WriteString("/running        显示运行中的会话\n")
	sb.WriteString("/help           显示本帮助\n")
	sb.WriteString("\n直接发送消息即可与 AI 对话。")
	return "info", "帮助", sb.String()
}

// cmdRunning lists all currently active turns across all chats.
func (h *Handler) cmdRunning(_, _ string) (level, title, body string) {
	sessions := h.RunningSessions()
	if len(sessions) == 0 {
		return "info", "运行中会话", "当前没有运行中的会话。"
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🔄 **运行中会话** (%d)\n\n", len(sessions)))
	for _, s := range sessions {
		sb.WriteString(fmt.Sprintf("- 群ID：`%s`（运行 %s）\n", s.ChatID, formatDuration(s.Duration)))
	}
	sb.WriteString("\n💡 如需中止，请到对应群内发送 `/session-abort`")
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
