// Package streamarchive writes per-run raw stream archives to disk with
// bounded retention. Each backend (claude/opencode) lands its run captures
// under {stateDir}/streams/{backend}/, pruned independently so a burst in
// one backend never evicts another's recent archives.
//
// The sink is best-effort: any setup failure (mkdir, open) returns nil and is
// logged by the caller, so archiving never fails a run.
//
// # Sensitive content
//
// The archive captures the CLI stdout verbatim (minus claude's thinking_tokens
// lines, which the bridge drops as inert). That includes the user's prompt,
// any file contents the agent reads (via a Read tool), and the model's reply.
// The files are created 0600, but if {stateDir} is included in backups, log
// forwarding, or shared snapshots this content leaves the host with them.
//
// To disable archiving entirely, set stream_history<=0 in the config (per
// backend) or leave state_dir empty. The DebugRedact flag affects only log
// output, NOT the archive.
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

// maxArchiveFileBytes caps a single archive file's size. Prune rotates by file
// COUNT at run start, not by size, so a runaway model output loop within one
// session (or a tool dumping a huge file) would otherwise grow a single file
// without bound until the run ends — multiplied by the independent per-backend
// dirs. Above the cap the file is sealed with a marker line and further writes
// are dropped so the run completes without filling the disk.
const maxArchiveFileBytes = 100 << 20 // 100 MiB

// NewSink opens (creating) the per-run archive file under
// {stateDir}/streams/{backend}/ and prunes that backend's directory to
// history-1 files first so the total stays bounded.
//
// Returns (nil, nil) when archiving is disabled (history<=0 or stateDir==""),
// or when setup fails — archiving is best-effort. The returned closer closes
// the file; callers defer it. backend is the subdirectory name (e.g. "miniagent");
// it is sanitized so an unexpected value cannot escape the
// streams root.
func NewSink(logger *log.Logger, stateDir, backend, chatID, replyToID string, history int, redact bool) (io.Writer, func() error) {
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
	if redact {
		// Redactor outside, cappedWriter inside: the cap counts on-disk
		// (post-redaction) bytes and the truncation marker is written
		// straight to the file, bypassing the redactor's line buffer.
		rw := NewRedactingWriter(newCappedWriter(f, maxArchiveFileBytes))
		return rw, func() error {
			// Flush the redactor's buffered partial line before closing the
			// file, or a final non-newline-terminated record would be lost.
			flushErr := rw.Close()
			closeErr := f.Close()
			if flushErr != nil {
				return flushErr
			}
			return closeErr
		}
	}
	return newCappedWriter(f, maxArchiveFileBytes), f.Close
}

// Prune deletes the oldest *.jsonl files in dir until at most keep remain.
// Best-effort: listing or unlink errors are logged and skipped so a transient
// FS failure never blocks a run. Filenames sort chronologically because they
// begin with fileTimeLayout.
func Prune(logger *log.Logger, dir string, keep int) {
	if keep < 0 {
		// A negative keep would make len(names)-keep overshoot the slice and
		// panic below. The sole caller passes history-1 with history>0, but
		// clamp at the entry point so a future caller cannot crash the run.
		keep = 0
	}
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

// cappedWriter passes bytes through to dst until cumulative bytes reach cap;
// then it writes a single truncation marker line and silently drops the rest.
// Dropping (not erroring) keeps archiving best-effort: the run is unaffected,
// only the archive tail is lost. Not safe for concurrent use; each archive
// sink is single-writer from one pump goroutine. Sits below the redactor (if
// any) so the cap counts on-disk bytes and redaction still runs on what fits.
type cappedWriter struct {
	dst     io.Writer
	limit   int64
	written int64
	sealed  bool
}

func newCappedWriter(dst io.Writer, limit int64) *cappedWriter {
	return &cappedWriter{dst: dst, limit: limit}
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.sealed {
		// Pretend the bytes were accepted so the caller's pump never errors or
		// retries on a full archive; the run must not be disturbed.
		return len(p), nil
	}
	if w.written+int64(len(p)) > w.limit {
		allow := w.limit - w.written
		if allow > 0 {
			n, err := w.dst.Write(p[:allow])
			w.written += int64(n)
			if err != nil {
				return n, err
			}
		}
		_, _ = fmt.Fprintf(w.dst, "\n[archive truncated: single-file cap %d MiB reached]\n", w.limit>>20)
		w.sealed = true
		return len(p), nil
	}
	n, err := w.dst.Write(p)
	w.written += int64(n)
	return n, err
}
