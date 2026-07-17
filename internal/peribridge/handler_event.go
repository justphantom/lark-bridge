package peribridge

import (
	"context"
	"fmt"

	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/cmdutil"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
)

// HandleEvent is the backend's entry point for each protocol.Event read from
// the frontend SSE stream. It routes by Type and emits Control messages back
// through the IPC client.
func (h *Handler) HandleEvent(ctx context.Context, ev *protocol.Event) error {
	if err := ev.Validate(); err != nil {
		h.Logger.Warn("invalid event from frontend",
			log.FieldError, err,
			log.FieldEventType, ev.Type)
		return err
	}
	switch ev.Type {
	case protocol.TypePrompt:
		return h.handlePromptEvent(ctx, ev)
	case protocol.TypeAnswer:
		// An interactive card reply (/cd picker). Route it to the goroutine
		// that emitted the Question control and is blocked in askAndWait.
		if ev.Answer != nil {
			h.Answers.Deliver(ev.Answer.RequestID, ev.Answer)
		}
		return nil
	case protocol.TypeAbort:
		if ev.Abort != nil {
			h.abortChat(ev.Abort.ChatID)
		}
		return nil
	case protocol.TypePing:
		return nil
	default:
		return fmt.Errorf("unknown event type %q", ev.Type)
	}
}

// handlePromptEvent resolves/creates the chat binding (carrying per-prompt
// overrides modelSpec onto the binding), then launches runPrompt. On binding
// failure or a busy chat it emits a Notice control so the frontend surfaces it
// instead of the prompt hanging.
func (h *Handler) handlePromptEvent(ctx context.Context, ev *protocol.Event) error {
	p := ev.Prompt
	if p == nil || p.Text == "" || p.ChatID == "" {
		return fmt.Errorf("invalid prompt event")
	}
	chatID := p.ChatID
	replyToID := ev.PromptID

	// Frontend only intercepts /backend; every other slash command is
	// forwarded verbatim as Text and parsed here, unless /skill was used.
	if !p.Skill {
		if cmd, _ := cmdutil.ParseCommand(p.Text); cmd != "" {
			bridgebase.GoSafe(h.Logger, "dispatchCommand:"+chatID, func() {
				h.dispatchCommand(h.AppCtx, chatID, p.Text, replyToID)
			})
			return nil
		}
	}

	binding, err := h.ensureBinding(chatID, p.Directory, p.ModelSpec)
	if err != nil {
		return h.emit(ctx, replyToID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "error", Title: "会话初始化失败", Message: err.Error()},
		})
	}

	// A binding without a working directory means the user has not picked a
	// project yet. Intercept before starting the CLI.
	if binding.Directory == "" {
		return h.emit(ctx, replyToID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{
				Level:   "warning",
				Title:   "请先选择工作目录",
				Message: "尚未设置工作目录。发送 `/cd` 选择一个项目目录后再开始对话。",
			},
		})
	}

	if p.ModelSpec != "" {
		h.Router.SetModelSpec(chatID, p.ModelSpec)
		binding.ModelSpec = p.ModelSpec
	}
	// Per-prompt overrides land on the binding; runPrompt reads them back when
	// constructing peri.RunOptions.
	if p.Permission != "" {
		h.Router.SetPermissionMode(chatID, p.Permission)
		binding.PermissionMode = p.Permission
	}
	if p.Effort != "" {
		h.Router.SetEffortLevel(chatID, p.Effort)
		binding.EffortLevel = p.Effort
	}
	if p.SettingsFile != "" {
		if err := validateSettingsPath(p.SettingsFile); err != nil {
			return h.emit(ctx, replyToID, &protocol.Control{
				Type:   protocol.TypeNotice,
				ChatID: chatID,
				Notice: &protocol.NoticePayload{Level: "error", Title: "settings 路径非法", Message: err.Error()},
			})
		}
		h.Router.SetSettingsFile(chatID, p.SettingsFile)
		binding.SettingsFile = p.SettingsFile
	}

	promptCtx, mine, ok := h.startPrompt(ctx, chatID)
	if !ok {
		return h.emit(ctx, replyToID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "warning", Title: "请稍后", Message: "正在处理上一个请求"},
		})
	}
	h.Wg.Add(1)
	go h.runPrompt(promptCtx, chatID, binding, p.Text, replyToID, mine)
	return nil
}
