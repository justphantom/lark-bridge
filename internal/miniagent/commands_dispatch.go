package miniagent

import (
	"context"
	"errors"
	"fmt"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// EmitFunc is the emit signature a command dispatcher uses to ship its reply
// (and any picker cards) to the frontend: promptID scopes the control to an
// in-flight turn (empty for a standalone card).
type EmitFunc func(ctx context.Context, promptID string, ctrl *protocol.Control) error

// CommandHandler runs one slash command bound to this handler. ctx is bounded
// by cmdutil.Timeout and carries the triggering message's ID (see ReplyToID).
type CommandHandler func(h *Handler, ctx context.Context, chatID string, args []string) (cmdutil.Result, error)

// replyToIDKey is the ctx key under which Dispatch stamps the triggering
// message's ID.
type replyToIDKey struct{}

// ReplyToID returns the ID of the user message that triggered the command,
// stamped on the handler ctx by Dispatch. Picker-style handlers (/model, /cd…)
// pass it as their Question card's promptID so the frontend can morph the
// progress card it already opened for that message into the picker card,
// keeping the whole interaction on one card. Empty outside Dispatch.
func ReplyToID(ctx context.Context) string {
	id, _ := ctx.Value(replyToIDKey{}).(string)
	return id
}

// CommandSpec is one slash command's metadata plus its handler. The display
// metadata (Name/Summary/Args/Title/Level) is shared infrastructure from
// cmdutil.Spec; Handler binds this handler.
type CommandSpec struct {
	cmdutil.Spec

	Handler CommandHandler
}

// Commands is the slash-command table plus the dispatch machinery.
type Commands struct {
	specs    []CommandSpec
	registry map[string]*CommandSpec
}

// NewCommands builds the registry derived from specs.
func NewCommands(specs []CommandSpec) *Commands {
	registry := make(map[string]*CommandSpec, len(specs))
	for i := range specs {
		registry[specs[i].Name] = &specs[i]
	}
	return &Commands{specs: specs, registry: registry}
}

// Lookup returns the spec for a command name, if any.
func (c *Commands) Lookup(name string) (*CommandSpec, bool) {
	spec, ok := c.registry[name]
	return spec, ok
}

// RenderHelp is the source of /help's body.
func (c *Commands) RenderHelp() string {
	specs := make([]cmdutil.Spec, 0, len(c.specs))
	for _, s := range c.specs {
		specs = append(specs, s.Spec)
	}
	return cmdutil.RenderHelp(specs)
}

// Dispatch runs one slash command and emits its reply as a TypeNotice
// Control. It is invoked by handleSessionCommand when the prompt text starts
// with "/".
func (c *Commands) Dispatch(h *Handler, emit EmitFunc, logger *log.Logger, parentCtx context.Context, chatID, prompt, replyToID string) {
	ctx, cancel := context.WithTimeout(parentCtx, cmdutil.Timeout)
	defer cancel()
	ctx = context.WithValue(ctx, replyToIDKey{}, replyToID)

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
		} else if res.Level != "" {
			level = res.Level
		}
		title = spec.Title
		if title == "" {
			title = spec.Name
		}
		if res.Title != "" {
			title = res.Title
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
