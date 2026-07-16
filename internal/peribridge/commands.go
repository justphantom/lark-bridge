package peribridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/hu/lark-bridge/internal/cmdutil"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
)

// commandResult is the body a slash command returns; an alias of cmdutil.Result.
type commandResult = cmdutil.Result

// commandHandler runs one slash command. ctx is bounded by cmdutil.Timeout.
type commandHandler func(h *Handler, ctx context.Context, chatID string, args []string) (commandResult, error)

// commandSpec is one slash command's metadata plus its handler.
type commandSpec struct {
	cmdutil.Spec
	Handler commandHandler
}

// commandSpecs is the source of truth for every slash command.
//
// peri is stateless, so the command set is trimmed relative to opencode-back:
// /model and /agent have no picker (peri exposes no list subcommand — models
// are configured in ~/.peri/settings.json); /session-new and /session-del
// reset the working context (directory + model pin) instead of a session id.
var commandSpecs []commandSpec

// commandRegistry is derived from commandSpecs in init().
var commandRegistry map[string]*commandSpec

func init() {
	commandSpecs = []commandSpec{
		{Spec: cmdutil.Spec{Name: "/running", Summary: "显示所有运行中的 peri 会话",
			Level: "info"}, Handler: (*Handler).cmdRunning},
		{Spec: cmdutil.Spec{Name: "/session-new", Summary: "重置工作上下文（保留目录，清除模型设置）",
			Title: "已重置", Level: "success"}, Handler: (*Handler).cmdSessionNew},
		{Spec: cmdutil.Spec{Name: "/session-abort", Summary: "中止当前正在执行的 peri 调用",
			Title: "已请求中止", Level: "success"}, Handler: (*Handler).cmdSessionAbort},
		{Spec: cmdutil.Spec{Name: "/session-del", Summary: "删除当前群的绑定（下次提问重建目录与设置）",
			Title: "已删除", Level: "success"}, Handler: (*Handler).cmdSessionDel},
		{Spec: cmdutil.Spec{Name: "/current", Summary: "显示当前目录与模型",
			Level: "info"}, Handler: (*Handler).cmdCurrent},
		{Spec: cmdutil.Spec{Name: "/model", Summary: "设置模型别名；传 clear 清除",
			Args: "[model|clear]", Title: "已切换模型", Level: "success"}, Handler: (*Handler).cmdModel},
		{Spec: cmdutil.Spec{Name: "/cd", Summary: "切换工作目录；不带参数弹出选择；传 clear 清除",
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
// Control.
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
