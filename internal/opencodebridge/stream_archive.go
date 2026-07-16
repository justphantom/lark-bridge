package opencodebridge

import (
	"io"

	"github.com/hu/lark-bridge/internal/streamarchive"
)

// backendTag is this backend's archive subdirectory name under
// {stateDir}/streams/. Kept as a const so the path stays single-sourced.
const backendTag = "opencode"

// newStreamSink delegates to streamarchive.NewSink with the opencode backend
// tag. opencode emits no high-volume discardable line type (unlike claude's
// thinking_tokens), so no wrapper filter is needed here.
func (h *Handler) newStreamSink(chatID, replyToID string) (io.Writer, func() error) {
	return streamarchive.NewSink(h.logger, h.stateDir, backendTag, chatID, replyToID, h.streamHistory)
}
