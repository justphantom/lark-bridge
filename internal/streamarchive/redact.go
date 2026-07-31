package streamarchive

import (
	"bytes"
	"encoding/json"
	"io"
)

// sensitiveFields lists JSON keys whose values should be redacted in
// stream archives when redaction is enabled. Covers user input side
// (prompt, tool input, Read output) while preserving the archive's
// debugging value for model replies.
var sensitiveFields = map[string]struct{}{
	"prompt":    {},
	"text":      {},
	"content":   {},
	"input":     {},
	"output":    {},
	"file_text": {},
	"thinking":  {},
}

// RedactingWriter wraps an io.Writer, redacting known-sensitive JSON fields
// in each NDJSON line before writing. Writes are buffered internally until a
// full line (terminated by '\n') has accumulated, so callers are NOT required
// to align each Write to a record boundary: the CLI pump may split a record
// across Writes or merge several into one. Complete lines are redacted and
// forwarded individually; a trailing partial line is held for the next Write
// and flushed by Close. Without this buffering a non-line-aligned Write would
// fail json.Unmarshal and land on disk verbatim — i.e. unredacted.
//
// Best-effort: unparseable lines pass through verbatim (matching the
// archive's "never block a run" contract). Not safe for concurrent use; each
// archive sink is single-writer from one pump goroutine.
//
// D2 note: redacted lines are SEMANTICALLY equivalent JSON, not byte
// identical to the CLI's original stdout — the unmarshal→marshal round trip
// normalises key order, whitespace, unicode escapes and number formatting.
// Tools that diff archives byte-for-byte against raw CLI output must run
// with archiving unredacted; this is a deliberate privacy-over-verbatim
// tradeoff (an in-place byte-substitution redactor is registered as a
// future evaluation item).
type RedactingWriter struct {
	W io.Writer
	// pending holds the bytes of a not-yet-terminated line across Writes.
	pending []byte
}

func NewRedactingWriter(w io.Writer) *RedactingWriter {
	return &RedactingWriter{W: w}
}

func (rw *RedactingWriter) Write(p []byte) (int, error) {
	rw.pending = append(rw.pending, p...)
	consumed := 0
	for {
		i := bytes.IndexByte(rw.pending[consumed:], '\n')
		if i < 0 {
			break
		}
		if err := rw.writeRedacted(rw.pending[consumed:consumed+i], true); err != nil {
			return 0, err
		}
		consumed += i + 1
	}
	// Compact the buffer so repeated partial-line Writes do not leak the
	// consumed prefix's capacity.
	rw.pending = append(rw.pending[:0], rw.pending[consumed:]...)
	// Report len(p), not the redacted length: the io.Writer contract counts
	// bytes consumed from p, and redaction changes the length.
	return len(p), nil
}

// Close flushes any buffered partial line — best-effort redacted like a
// complete line, but without appending a newline the original never had — to
// the underlying writer. It does NOT close the underlying writer; ownership
// stays with the caller (the archive sink closes the file itself).
func (rw *RedactingWriter) Close() error {
	if len(rw.pending) == 0 {
		return nil
	}
	line := rw.pending
	rw.pending = nil
	return rw.writeRedacted(line, false)
}

func (rw *RedactingWriter) writeRedacted(line []byte, newline bool) error {
	out := redactLine(line)
	if newline {
		// Preserve the NDJSON framing: Marshal strips the newline, and
		// without restoring it the archive would collapse into one line.
		out = append(out, '\n')
	}
	_, err := rw.W.Write(out)
	return err
}

// redactLine returns the line with sensitive fields redacted when it parses
// as a JSON object; anything else passes through verbatim.
func redactLine(line []byte) []byte {
	var data map[string]any
	if err := json.Unmarshal(line, &data); err != nil {
		return line
	}
	redactMap(data)
	redacted, err := json.Marshal(data)
	if err != nil {
		// Marshal should never fail on a map[string]any we just
		// unmarshalled, but fall back to verbatim just in case.
		return line
	}
	return redacted
}

func redactMap(m map[string]any) {
	for k, v := range m {
		if _, ok := sensitiveFields[k]; ok {
			m[k] = "[REDACTED]"
			continue
		}
		redactValue(v)
	}
}

// redactValue recurses into nested objects and arrays of any depth, so
// objects hidden inside "array of arrays" (e.g. content blocks wrapped by a
// CLI that batches events) are redacted the same as top-level ones.
func redactValue(v any) {
	switch t := v.(type) {
	case map[string]any:
		redactMap(t)
	case []any:
		for _, item := range t {
			redactValue(item)
		}
	}
}
