package claudebridge

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// streamSubdir is the directory under stateDir holding one file per CLI run.
const streamSubdir = "streams"

// streamFileTimeLayout formats the run start time into the leading filename
// segment. It is lexicographically ordered, so sorting filenames equals
// chronological order — the rotation prune relies on this.
const streamFileTimeLayout = "20060102T150405.000000000"

// newStreamSink opens (creating) the per-run archive file under
// {stateDir}/streams and prunes the directory to streamHistory-1 files first
// so the total stays bounded. Returns nil when archiving is disabled
// (streamHistory<=0) or when setup fails — archiving is best-effort and must
// never fail the run. The returned closer closes the file; callers defer it.
func (h *Handler) newStreamSink(chatID, replyToID string) (io.Writer, func() error) {
	if h.streamHistory <= 0 || h.stateDir == "" {
		return nil, nil
	}
	dir := filepath.Join(h.stateDir, streamSubdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.logger.Warn("stream archive: mkdir", log.FieldError, err)
		return nil, nil
	}
	pruneStreamDir(h.logger, dir, h.streamHistory-1)

	name := fmt.Sprintf("%s_%s_%s.jsonl",
		time.Now().UTC().Format(streamFileTimeLayout),
		sanitizeName(chatID),
		sanitizeName(replyToID))
	// O_APPEND so a name collision (same chat+reply, same nanosecond) folds
	// into one file rather than clobbering prior bytes.
	f, err := os.OpenFile(filepath.Join(dir, name),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.logger.Warn("stream archive: open", log.FieldError, err)
		return nil, nil
	}
	return f, f.Close
}

// pruneStreamDir deletes the oldest *.jsonl files in dir until at most keep
// remain. Best-effort: listing or unlink errors are logged and skipped so a
// transient FS failure never blocks a run. Filenames sort chronologically
// because they begin with streamFileTimeLayout.
func pruneStreamDir(logger *log.Logger, dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Warn("stream archive: readdir", log.FieldError, err)
		return
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	// Lexical order = oldest first (timestamp-prefixed names).
	sort.Strings(names)
	for _, n := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			logger.Warn("stream archive: remove", log.FieldError, err)
		}
	}
}

// sanitizeName collapses any character unsafe for a filename into '_' so a
// chat/reply id with an unexpected character cannot escape streamSubdir or
// break path semantics.
func sanitizeName(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
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
