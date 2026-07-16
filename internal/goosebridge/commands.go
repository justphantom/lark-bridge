package goosebridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/hu/lark-bridge/internal/cmdutil"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
)

// commandResult is the body a slash command returns; an alias of cmdutil.Result
// so handler signatures stay short at the call sites.
type commandResult = cmdutil.Result

// commandHandler runs one slash command. ctx is bounded by cmdutil.Timeout.
type commandHandler func(h *Handler, ctx context.Context, chatID string, args []string) (commandResult, error)

// commandSpec is one slash command's metadata plus its handler. The display
// metadata (Name/Summary/Args/Title/Level) is shared infrastructure from
// cmdutil.Spec; the Handler is bridge-specific because it binds *Handler.
type commandSpec struct {
	cmdutil.Spec
	Handler commandHandler
}

// commandSpecs is the source of truth for every slash command.
var commandSpecs []commandSpec

// commandRegistry is derived from commandSpecs in init().
var commandRegistry map[string]*commandSpec

func init() {
	commandSpecs = []commandSpec{
		{Spec: cmdutil.Spec{Name: "/running", Summary: "显示所有运行中的 goose 会话",
			Level: "info"}, Handler: (*Handler).cmdRunning},
		{Spec: cmdutil.Spec{Name: "/session-list", Summary: "列出本群绑定的 goose 会话",
			Level: "info"}, Handler: (*Handler).cmdListSessions},
		{Spec: cmdutil.Spec{Name: "/session-new", Summary: "开启新的 goose 对话（保留工作目录，重置上下文）",
			Title: "已开启新对话", Level: "success"}, Handler: (*Handler).cmdSessionNew},
		{Spec: cmdutil.Spec{Name: "/session-abort", Summary: "中止当前正在执行的 goose 调用",
			Title: "已请求中止", Level: "success"}, Handler: (*Handler).cmdSessionAbort},
		{Spec: cmdutil.Spec{Name: "/session-del", Summary: "删除当前群绑定的会话（下次提问会重建）",
			Title: "已删除", Level: "success"}, Handler: (*Handler).cmdSessionDel},
		{Spec: cmdutil.Spec{Name: "/current", Summary: "显示当前会话、目录与模型",
			Level: "info"}, Handler: (*Handler).cmdCurrent},
		{Spec: cmdutil.Spec{Name: "/model", Summary: "设置模型；不带参数弹出选择；传 clear 清除",
			Args: "[model|clear]", Title: "已切换模型", Level: "success"}, Handler: (*Handler).cmdModel},
		{Spec: cmdutil.Spec{Name: "/cd", Summary: "切换工作目录（重置会话）；不带参数弹出选择；传 clear 清除",
			Args: "[dir|clear]", Title: "已切换目录", Level: "success"}, Handler: (*Handler).cmdDirectory},
		{Spec: cmdutil.Spec{Name: "/help", Summary: "显示本帮助",
			Level: "info"}, Handler: (*Handler).cmdHelp},
	}
	commandRegistry = make(map[string]*commandSpec, len(commandSpecs))
	for i := range commandSpecs {
		commandRegistry[commandSpecs[i].Name] = &commandSpecs[i]
	}
}

// dispatchCommand runs one slash command and emits its reply as a TypeNotice
// Control. It is invoked by handlePromptEvent when the prompt text starts
// with "/".
func (h *Handler) dispatchCommand(parentCtx context.Context, chatID, prompt, replyToID string) {
	ctx, cancel := context.WithTimeout(parentCtx, cmdutil.Timeout)
	defer cancel()

	cmd, args := cmdutil.ParseCommand(prompt)
	var title, body, level string
	var res commandResult
	var handlerErr error
	spec, ok := commandRegistry[cmd]
	if !ok {
		title = cmd
		body = fmt.Sprintf("未知命令 %q。\n%s", title, renderCmdHelp())
		level = "warning"
	} else {
		res, handlerErr = spec.Handler(h, ctx, chatID, args)
		body = res.Body
		level = spec.Level
		if handlerErr != nil {
			if errors.Is(handlerErr, context.DeadlineExceeded) {
				body = fmt.Sprintf("⚠️ 命令执行超时（>%s），请稍后重试。", cmdutil.Timeout)
				level = "warning"
			} else {
				body = fmt.Sprintf("⚠️ %v", handlerErr)
				level = "error"
			}
		}
		title = spec.Title
		if title == "" {
			title = spec.Name
		}
	}
	if title == "" {
		title = "命令结果"
	}

	// A handler that drove its own interaction (e.g. emitted a Question card
	// and waited for the answer) signals Handled so the dispatcher does not
	// also fire a TypeNotice. An error always overrides: the dispatcher must
	// surface failures even from a self-handling command.
	if res.Handled && handlerErr == nil {
		return
	}

	if err := h.emit(ctx, replyToID, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: body,
			Field: res.Field, Before: res.Before, After: res.After},
	}); err != nil {
		h.logger.Debug("emit command reply", log.FieldChatID, chatID, log.FieldError, err)
	}
}

// renderCmdHelp is the source of /help's body.
func renderCmdHelp() string {
	specs := make([]cmdutil.Spec, 0, len(commandSpecs))
	for _, s := range commandSpecs {
		specs = append(specs, s.Spec)
	}
	return cmdutil.RenderHelp(specs)
}
