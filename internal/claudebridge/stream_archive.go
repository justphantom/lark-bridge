package claudebridge

import (
	"bytes"
	"io"

	"github.com/hu/lark-bridge/internal/streamarchive"
)

// backendTag is this backend's archive subdirectory name under
// {stateDir}/streams/. Kept as a const so the path and any log/debug mention
// of the backend stay single-sourced.
const backendTag = "claude"

// newStreamSink wraps streamarchive.NewSink with the claude backend tag. The
// generic sink/prune/sanitize logic lives in internal/streamarchive; this
// method only adds the backend identity and the claude-specific line filter.
func (h *Handler) newStreamSink(chatID, replyToID string) (io.Writer, func() error) {
	return streamarchive.NewSink(h.logger, h.stateDir, backendTag, chatID, replyToID, h.streamHistory)
}

// thinkingTokensMarker identifies a system line the bridge never consumes but
// upstream emits on every reasoning-token delta — the bulk of the archive by
// volume (实测 88%+ of claude stream lines). Matched as a substring rather than
// decoding JSON: the marker is specific enough, and decoding each line just to
// drop most of them would waste the saving. Kept as a var so a future "keep
// the budget signal" toggle can flip the filter off without code surgery.
var thinkingTokensMarker = []byte(`"subtype":"thinking_tokens"`)

// thinkingTokensFilter wraps an archive sink and drops thinking_tokens lines.
// pump writes one JSON line per Write call (line + "\n"), so a substring check
// per Write cleanly classifies whole lines without splitting. Dropped writes
// still report success (n = len(p)) so the producer sees no short-write.
type thinkingTokensFilter struct{ w io.Writer }

func (f *thinkingTokensFilter) Write(p []byte) (int, error) {
	if bytes.Contains(p, thinkingTokensMarker) {
		return len(p), nil
	}
	return f.w.Write(p)
}

// wrapThinkingFilter wraps w so the archive omits thinking_tokens lines. A nil w
// returns nil (archiving disabled) so the caller can chain it unconditionally.
func wrapThinkingFilter(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &thinkingTokensFilter{w: w}
}
