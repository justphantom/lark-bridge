package peribridge

import (
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
// segment. Lexicographic order equals chronological order.
const streamFileTimeLayout = "20060102T150405.000000000"

// newStreamSink opens (creating) the per-run archive file under
// {stateDir}/streams and prunes the directory to streamHistory-1 files first.
// Returns nil when archiving is disabled (streamHistory<=0) or when setup
// fails — archiving is best-effort and must never fail the run.
//
// NOTE: peri client.RunOptions has no LineSink field yet (unlike opencode), so
// this sink is currently unused by runPeri. It is retained so wiring a LineSink
// into the peri client later is a one-line change in runPeri.
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
	f, err := os.OpenFile(filepath.Join(dir, name),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		h.logger.Warn("stream archive: open", log.FieldError, err)
		return nil, nil
	}
	return f, f.Close
}

// pruneStreamDir deletes the oldest *.jsonl files in dir until at most keep
// remain.
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
	sort.Strings(names)
	for _, n := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			logger.Warn("stream archive: remove", log.FieldError, err)
		}
	}
}

// sanitizeName collapses any character unsafe for a filename into '_'.
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
