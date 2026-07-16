package goosebridge

import (
	"io"

	"github.com/hu/lark-bridge/internal/streamarchive"
)

// backendTag is this backend's archive subdirectory name under
// {stateDir}/streams/. Kept as a const so the path and any log/debug mention
// of the backend stay single-sourced.
const backendTag = "goose"

// newStreamSink wraps streamarchive.NewSink with the goose backend tag. The
// generic sink/prune/sanitize logic lives in internal/streamarchive; this
// method only adds the backend identity.
func (h *Handler) newStreamSink(chatID, replyToID string) (io.Writer, func() error) {
	return streamarchive.NewSink(h.logger, h.stateDir, backendTag, chatID, replyToID, h.streamHistory)
}
