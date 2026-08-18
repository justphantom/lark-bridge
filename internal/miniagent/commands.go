package miniagent

import (
	"context"
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// commands is the single source of truth for slash-command metadata and
// handlers. It uses the in-package Commands machinery (commands_dispatch.go)
// for parsing, help rendering, and dispatch.
//
// /running and /abort are registered so /help lists them, but they are
// dispatched earlier in HandleEvent (before startTurn) because they must not
// occupy the per-chat turn slot.
var commands *Commands

func init() {
	commands = NewCommands([]CommandSpec{
		{Spec: cmdutil.Spec{Name: "/current", Summary: "显示当前模型/工作目录/思考/会话",
			Title: "当前状态", Level: "info"}, Handler: (*Handler).cmdCurrentBridge},
		{Spec: cmdutil.Spec{Name: "/model", Summary: "切换模型；不带参数弹出选择；传 clear 清除",
			Args: "[model|clear]", Title: "已切换模型", Level: "success"}, Handler: (*Handler).cmdModelBridge},
		{Spec: cmdutil.Spec{Name: "/cd", Summary: "切换工作目录；不带参数弹出选择；传 clear 清除",
			Args: "[dir|clear]", Title: "已切换目录", Level: "success"}, Handler: (*Handler).cmdDirectoryBridge},
		{Spec: cmdutil.Spec{Name: "/config", Summary: "切换配置文件；不带参数弹出选择；传 clear 清除",
			Args: "[clear]", Title: "已切换配置文件", Level: "success"}, Handler: (*Handler).cmdConfigBridge},
		{Spec: cmdutil.Spec{Name: "/effort", Summary: "设置思考级别；不带参数弹出选择；传 clear 清除",
			Args: "[level|clear]", Title: "已切换思考级别", Level: "success"}, Handler: (*Handler).cmdEffortBridge},
		{Spec: cmdutil.Spec{Name: "/maxiter", Summary: "设置每轮 LLM 调用上限；不带参数显示当前；传 clear 清除",
			Args: "[N|clear]", Title: "迭代上限", Level: "info"}, Handler: (*Handler).cmdMaxIterBridge},
		{Spec: cmdutil.Spec{Name: "/new", Summary: "清空当前会话历史（下次提问开始新会话）",
			Title: "已清除会话", Level: "success"}, Handler: (*Handler).cmdNewBridge},
		{Spec: cmdutil.Spec{Name: "/send", Summary: "发送工作目录中的文件到本群；不带参数弹出目录选择",
			Args: "[relative-path]", Title: "发送文件", Level: "info"}, Handler: (*Handler).cmdSendBridge},
		{Spec: cmdutil.Spec{Name: "/pull", Summary: "在当前工作目录执行 git pull --ff-only",
			Title: "拉取", Level: "info"}, Handler: (*Handler).cmdPullBridge},
		{Spec: cmdutil.Spec{Name: "/push", Summary: "在当前工作目录执行 git push",
			Title: "推送", Level: "info"}, Handler: (*Handler).cmdPushBridge},
		{Spec: cmdutil.Spec{Name: "/build", Summary: "在当前工作目录执行 make build",
			Title: "构建", Level: "info"}, Handler: (*Handler).cmdBuildBridge},
		{Spec: cmdutil.Spec{Name: "/running", Summary: "显示所有运行中的 miniagent 会话",
			Title: "运行中会话", Level: "info"}, Handler: (*Handler).cmdRunningBridge},
		{Spec: cmdutil.Spec{Name: "/abort", Summary: "中止当前正在执行的任务",
			Title: "已请求中止", Level: "success"}, Handler: (*Handler).cmdAbortBridge},
		{Spec: cmdutil.Spec{Name: "/help", Summary: "显示本帮助",
			Title: "帮助", Level: "info"}, Handler: (*Handler).cmdHelpBridge},
	})
}

// renderCmdHelp is the source of /help's body.
func renderCmdHelp() string { return commands.RenderHelp() }

// isSessionCommand reports whether prompt is one this handler owns. It never
// panics on a bare "/" — strings.Fields collapses that to nothing.
// /running and /abort are deliberately excluded: HandleEvent dispatches them
// before startTurn so they do not occupy the per-chat turn slot.
func isSessionCommand(prompt string) bool {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false
	}
	cmd := fields[0]
	if cmd == "/running" || cmd == "/abort" {
		return false
	}
	_, ok := commands.Lookup(cmd)
	return ok
}

// handleSessionCommand reserves the per-chat turn slot (so a command cannot
// race with an in-flight runTurn over the router binding), runs the command
// through the Commands dispatch, and replies via a Notice.
// A busy chat gets the same "处理中" notice a prompt would.
func (h *Handler) handleSessionCommand(ctx context.Context, chatID, promptID, prompt string) error {
	turnCtx, mine, ok := h.startTurn(ctx, chatID, promptID)
	if !ok {
		h.notifyWithPromptID(chatID, promptID, "warning", "处理中", "上一条消息还在处理，请等它结束后再发。")
		return nil
	}
	defer h.endTurn(chatID, mine)
	h.SetPromptIDForPickers(chatID, promptID)
	defer h.SetPromptIDForPickers(chatID, "")

	commands.Dispatch(h, h.emitNotice, h.logger, turnCtx, chatID, prompt, promptID)
	return nil
}

// emitNotice is the EmitFunc passed to Commands.Dispatch. It binds
// the notice to the command's own promptID so the frontend patches the
// progress card it opened for the triggering message, instead of leaving a
// stale card and sending a new one.
func (h *Handler) emitNotice(_ context.Context, promptID string, ctrl *protocol.Control) error {
	ctrl.PromptID = promptID
	h.sendCtrl(ctrl)
	return nil
}

// firstArg returns the first command argument, or "" when there is none.
func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// joinArgs reconstructs the remainder of the prompt with single spaces. This
// matches the legacy /send handling so paths with spaces are not truncated.
func joinArgs(args []string) string {
	return strings.Join(args, " ")
}

// --- bridge adapters: legacy (level,title,body) handlers → cmdutil.Result ---

func (h *Handler) cmdCurrentBridge(ctx context.Context, chatID string, _ []string) (cmdutil.Result, error) {
	level, title, body := h.cmdCurrent(ctx, chatID, "")
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdModelBridge(ctx context.Context, chatID string, args []string) (cmdutil.Result, error) {
	level, title, body := h.cmdModel(ctx, chatID, firstArg(args))
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdDirectoryBridge(ctx context.Context, chatID string, args []string) (cmdutil.Result, error) {
	level, title, body := h.cmdDirectory(ctx, chatID, firstArg(args))
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdConfigBridge(ctx context.Context, chatID string, args []string) (cmdutil.Result, error) {
	level, title, body := h.cmdConfig(ctx, chatID, firstArg(args))
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdEffortBridge(ctx context.Context, chatID string, args []string) (cmdutil.Result, error) {
	level, title, body := h.cmdEffort(ctx, chatID, firstArg(args))
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdMaxIterBridge(ctx context.Context, chatID string, args []string) (cmdutil.Result, error) {
	level, title, body := h.cmdMaxIter(ctx, chatID, firstArg(args))
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdNewBridge(ctx context.Context, chatID string, _ []string) (cmdutil.Result, error) {
	level, title, body := h.cmdNew(ctx, chatID, "")
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdSendBridge(ctx context.Context, chatID string, args []string) (cmdutil.Result, error) {
	level, title, body := h.cmdSend(ctx, chatID, joinArgs(args))
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdPullBridge(ctx context.Context, chatID string, _ []string) (cmdutil.Result, error) {
	level, title, body := h.cmdPull(ctx, chatID, "")
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdPushBridge(ctx context.Context, chatID string, _ []string) (cmdutil.Result, error) {
	level, title, body := h.cmdPush(ctx, chatID, "")
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdBuildBridge(ctx context.Context, chatID string, _ []string) (cmdutil.Result, error) {
	level, title, body := h.cmdBuild(ctx, chatID, "")
	if level == "async" {
		return cmdutil.Result{Handled: true}, nil
	}
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdRunningBridge(ctx context.Context, chatID string, _ []string) (cmdutil.Result, error) {
	level, title, body := h.cmdRunning(ctx, chatID, "")
	return cmdutil.Result{Body: body, Title: title, Level: level}, nil
}

func (h *Handler) cmdAbortBridge(_ context.Context, chatID string, _ []string) (cmdutil.Result, error) {
	if h.abortChat(chatID) {
		return cmdutil.Result{Body: "正在停止当前任务。", Title: "已请求中止", Level: "success"}, nil
	}
	return cmdutil.Result{Body: "当前没有正在执行的任务。", Title: "无可中止", Level: "info"}, nil
}

func (h *Handler) cmdHelpBridge(context.Context, string, []string) (cmdutil.Result, error) {
	return cmdutil.Result{Body: renderCmdHelp(), Title: "帮助", Level: "info"}, nil
}

// cmdCurrent reports the per-chat model + directory + config + thinking
// + session id the next fork will use. Falls back to the global defaults when
// the chat has no pin. The session id is the miniagent-generated id persisted
// for this chat (absent → next prompt creates a fresh session via -save-session).
func (h *Handler) cmdCurrent(_ context.Context, chatID, _ string) (level, title, body string) {
	sid := h.lookupSessionID(chatID)
	if sid == "" {
		sid = "无（下次提问新建）"
	}
	return "info", "当前状态", fmt.Sprintf("模型：%s\n工作目录：%s\n配置文件：%s\n思考级别：%s\n迭代上限：%s\n会话ID：%s",
		displayModel(h.activeProvider(chatID), h.activeModel(chatID)), h.activeDir(chatID), h.activeConfig(chatID), h.activeThinking(chatID), h.formatMaxIter(h.activeMaxIter(chatID)), sid)
}
