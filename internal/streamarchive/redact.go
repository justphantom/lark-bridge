package streamarchive

import (
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
// in each NDJSON line before writing. Best-effort: unparseable lines pass
// through verbatim (matching the archive's "never block a run" contract).
// Each line is written with a trailing newline.
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
}

func NewRedactingWriter(w io.Writer) *RedactingWriter {
	return &RedactingWriter{W: w}
}

func (rw *RedactingWriter) Write(p []byte) (int, error) {
	// Try to parse as JSON. If it fails, write verbatim.
	var data map[string]any
	if err := json.Unmarshal(p, &data); err != nil {
		return rw.W.Write(p)
	}
	redactMap(data)
	redacted, err := json.Marshal(data)
	if err != nil {
		// Marshal should never fail on a map[string]any we just
		// unmarshalled, but fall back to verbatim just in case.
		return rw.W.Write(p)
	}
	// Preserve the trailing newline of the NDJSON record: Marshal strips
	// it, and without restoring it the archive would collapse into one
	// giant line.
	if len(p) > 0 && p[len(p)-1] == '\n' {
		redacted = append(redacted, '\n')
	}
	// Report len(p), not len(redacted): the io.Writer contract counts
	// bytes consumed from p, and redaction changes the length.
	if _, err := rw.W.Write(redacted); err != nil {
		return 0, err
	}
	return len(p), nil
}

func redactMap(m map[string]any) {
	for k, v := range m {
		if _, ok := sensitiveFields[k]; ok {
			m[k] = "[REDACTED]"
			continue
		}
		// Recurse into nested objects.
		if sub, ok := v.(map[string]any); ok {
			redactMap(sub)
		}
		// Recurse into arrays.
		if arr, ok := v.([]any); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]any); ok {
					redactMap(sub)
				}
			}
		}
	}
}
