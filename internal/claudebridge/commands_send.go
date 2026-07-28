package claudebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
)

// cmdSend delivers a file from the chat's bound working directory into the
// chat via the frontend (send-file-design.md). No-arg opens the directory
// browser; a relative path sends directly. The backend only reads the file
// and emits a TypeFile control; the frontend owns the Feishu upload/send, so
// this backend holds no Feishu credentials. agent is bypassed entirely.
func (h *Handler) cmdSend(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	return bridgebase.CmdSend(ctx, h.Core, chatID, b.Directory, args)
}
