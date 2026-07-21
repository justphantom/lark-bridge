package opencodeservebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// commandResult is the body a slash command returns; an alias of cmdutil.Result
// so handler signatures stay short at the call sites.
type commandResult = cmdutil.Result

// commands is this backend's slash-command table. The dispatch machinery is
// shared by every bridge in bridgebase.Commands, generic over *Handler. The
// table is built in init() (not a var initializer) because the /help handler
// refers back to commands via renderCmdHelp — a var initializer would form an
// initialization cycle.
var commands *bridgebase.Commands[*Handler]

func init() {
	commands = bridgebase.NewCommands([]bridgebase.CommandSpec[*Handler]{
		{Spec: cmdutil.Spec{Name: "/running", Summary: "显示所有运行中的 opencode 会话",
			Level: "info"}, Handler: (*Handler).cmdRunning},
		{Spec: cmdutil.Spec{Name: "/session-new", Summary: "开启新的 opencode 对话（保留工作目录，重置上下文）",
			Title: "已开启新对话", Level: "success"}, Handler: (*Handler).cmdSessionNew},
		{Spec: cmdutil.Spec{Name: "/session-abort", Summary: "中止当前正在执行的 opencode 调用",
			Title: "已请求中止", Level: "success"}, Handler: (*Handler).cmdSessionAbort},
		{Spec: cmdutil.Spec{Name: "/session-del", Summary: "删除当前群绑定的会话（下次提问会重建）",
			Title: "已删除", Level: "success"}, Handler: (*Handler).cmdSessionDel},
		{Spec: cmdutil.Spec{Name: "/delete-idle", Summary: "删除所有未绑定且空闲的会话",
			Title: "已清理空闲会话", Level: "success"}, Handler: (*Handler).cmdDeleteIdleSessions},
		{Spec: cmdutil.Spec{Name: "/current", Summary: "显示当前会话、目录、模型与 agent",
			Level: "info"}, Handler: (*Handler).cmdCurrent},
		{Spec: cmdutil.Spec{Name: "/model", Summary: "设置模型；不带参数弹出选择；传 clear 清除",
			Args: "[model|clear]", Title: "已切换模型", Level: "success"}, Handler: (*Handler).cmdModel},
		{Spec: cmdutil.Spec{Name: "/agent", Summary: "设置 agent；不带参数弹出选择；传 clear 清除",
			Args: "[agent|clear]", Title: "已切换 agent", Level: "success"}, Handler: (*Handler).cmdAgent},
		{Spec: cmdutil.Spec{Name: "/cd", Summary: "切换工作目录（重置会话）；不带参数弹出选择；传 clear 清除",
			Args: "[dir|clear]", Title: "已切换目录", Level: "success"}, Handler: (*Handler).cmdDirectory},
		{Spec: cmdutil.Spec{Name: "/help", Summary: "显示本帮助",
			Level: "info"}, Handler: (*Handler).cmdHelp},
	})
}

// dispatchCommand runs one slash command and emits its reply as a TypeNotice
// Control. It is invoked by handlePromptEvent when the prompt text starts
// with "/".
func (h *Handler) dispatchCommand(parentCtx context.Context, chatID, prompt, replyToID string) {
	commands.Dispatch(h, h.emit, h.Logger, parentCtx, chatID, prompt, replyToID)
}

// renderCmdHelp is the source of /help's body.
func renderCmdHelp() string { return commands.RenderHelp() }
