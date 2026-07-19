// Package streamarchive writes per-run raw stream archives to disk with
// bounded retention. Each backend (claude/opencode) lands its run captures
// under {stateDir}/streams/{backend}/, pruned independently so a burst in
// one backend never evicts another's recent archives.
//
// The sink is best-effort: any setup failure (mkdir, open) returns nil and is
// logged by the caller, so archiving never fails a run.
package streamarchive

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// topDir is the directory under stateDir holding all per-backend archives.
const topDir = "streams"

// fileTimeLayout formats the run start time into the leading filename segment.
// It is lexicographically ordered, so sorting filenames equals chronological
// order — the rotation prune relies on this.
const fileTimeLayout = "20060102T150405.000000000"

// NewSink opens (creating) the per-run archive file under
// {stateDir}/streams/{backend}/ and prunes that backend's directory to
// history-1 files first so the total stays bounded.
//
// Returns (nil, nil) when archiving is disabled (history<=0 or stateDir==""),
// or when setup fails — archiving is best-effort. The returned closer closes
// the file; callers defer it. backend is the subdirectory name (e.g. "claude",
// "opencode"); it is sanitized so an unexpected value cannot escape the
// streams root.
func NewSink(logger *log.Logger, stateDir, backend, chatID, replyToID string, history int) (io.Writer, func() error) {
	if history <= 0 || stateDir == "" {
		return nil, nil
	}
	dir := filepath.Join(stateDir, topDir, SanitizeName(backend))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		logger.Warn("stream archive: mkdir", log.FieldError, err)
		return nil, nil
	}
	Prune(logger, dir, history-1)

	name := fmt.Sprintf("%s_%s_%s.jsonl",
		time.Now().UTC().Format(fileTimeLayout),
		SanitizeName(chatID),
		SanitizeName(replyToID))
	// O_APPEND so a name collision (same chat+reply, same nanosecond) folds
	// into one file rather than clobbering prior bytes.
	f, err := os.OpenFile(filepath.Join(dir, name),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logger.Warn("stream archive: open", log.FieldError, err)
		return nil, nil
	}
	return f, f.Close
}

// Prune deletes the oldest *.jsonl files in dir until at most keep remain.
// Best-effort: listing or unlink errors are logged and skipped so a transient
// FS failure never blocks a run. Filenames sort chronologically because they
// begin with fileTimeLayout.
func Prune(logger *log.Logger, dir string, keep int) {
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

// SanitizeName collapses any character unsafe for a filename into '_' so a
// chat/reply/backend id with an unexpected character cannot escape the archive
// directory or break path semantics.
func SanitizeName(s string) string {
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
