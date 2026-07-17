package claudebridge

import (
	"context"

	"github.com/hu/lark-bridge/internal/bridgebase"
)

// askAndWait runs the interactive picker loop. The implementation is shared
// by every backend bridge in bridgebase.AskAndWait; this wrapper only binds
// the handler's appCtx, answer broker, and emit.
func (h *Handler) askAndWait(
	chatID, replyToID, kind, label string,
	listFn func(context.Context) ([]string, error),
	allowCustom bool,
) (string, error) {
	return bridgebase.AskAndWait(h.AppCtx, h.Answers, h.emit, chatID, replyToID, kind, label, listFn, allowCustom)
}

// emitNotice sends a Notice control on the picker's own lifecycle; shared
// implementation lives in bridgebase.EmitNotice.
func (h *Handler) emitNotice(chatID, level, title, body string, extra ...string) error {
	return bridgebase.EmitNotice(h.AppCtx, h.emit, chatID, level, title, body, extra...)
}
