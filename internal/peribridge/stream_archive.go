package peribridge

import (
	"io"

	"github.com/hu/lark-bridge/internal/streamarchive"
)

// backendTag is this backend's archive subdirectory name under
// {stateDir}/streams/. Kept as a const so the path stays single-sourced.
const backendTag = "peri"

// newStreamSink delegates to streamarchive.NewSink with the peri backend tag.
// peri emits no high-volume discardable line type, so no wrapper filter is
// needed here.
func (h *Handler) newStreamSink(chatID, replyToID string) (io.Writer, func() error) {
	return streamarchive.NewSink(h.logger, h.stateDir, backendTag, chatID, replyToID, h.streamHistory)
}
