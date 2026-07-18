package bridgebase

import (
	"context"
	"errors"
	"fmt"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// CommandHandler runs one slash command bound to a bridge's handler type H.
// ctx is bounded by cmdutil.Timeout.
type CommandHandler[H any] func(h H, ctx context.Context, chatID string, args []string) (cmdutil.Result, error)

// CommandSpec is one slash command's metadata plus its handler. The display
// metadata (Name/Summary/Args/Title/Level) is shared infrastructure from
// cmdutil.Spec; the Handler binds the bridge's own handler type.
type CommandSpec[H any] struct {
	cmdutil.Spec
	Handler CommandHandler[H]
}

// Commands is a bridge's slash-command table plus the dispatch machinery,
// generic over the bridge's handler type so the logic lives here once.
type Commands[H any] struct {
	specs    []CommandSpec[H]
	registry map[string]*CommandSpec[H]
}

// NewCommands builds the registry derived from specs.
func NewCommands[H any](specs []CommandSpec[H]) *Commands[H] {
	registry := make(map[string]*CommandSpec[H], len(specs))
	for i := range specs {
		registry[specs[i].Name] = &specs[i]
	}
	return &Commands[H]{specs: specs, registry: registry}
}

// RenderHelp is the source of /help's body.
func (c *Commands[H]) RenderHelp() string {
	specs := make([]cmdutil.Spec, 0, len(c.specs))
	for _, s := range c.specs {
		specs = append(specs, s.Spec)
	}
	return cmdutil.RenderHelp(specs)
}

// Dispatch runs one slash command and emits its reply as a TypeNotice
// Control. It is invoked by the bridge's handlePromptEvent when the prompt
// text starts with "/".
func (c *Commands[H]) Dispatch(h H, emit EmitFunc, logger *log.Logger, parentCtx context.Context, chatID, prompt, replyToID string) {
	ctx, cancel := context.WithTimeout(parentCtx, cmdutil.Timeout)
	defer cancel()

	cmd, args := cmdutil.ParseCommand(prompt)
	var title, body, level string
	var res cmdutil.Result
	var handlerErr error
	spec, ok := c.registry[cmd]
	if !ok {
		title = cmd
		body = fmt.Sprintf("未知命令 %q。\n%s", title, c.RenderHelp())
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

	if err := emit(ctx, replyToID, &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{Level: level, Title: title, Message: body,
			Field: res.Field, Before: res.Before, After: res.After},
	}); err != nil {
		logger.Debug("emit command reply", log.FieldChatID, chatID, log.FieldError, err)
	}
}
