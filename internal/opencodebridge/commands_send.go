package opencodebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
)

// cmdSend delivers a file from the chat's bound working directory into the
// chat via the frontend (send-file-design.md). Mirrors claudebridge.cmdSend:
// backend reads + emits TypeFile, frontend uploads + sends.
func (h *Handler) cmdSend(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	return bridgebase.CmdSend(ctx, h.Core, chatID, b.Directory, args)
}
