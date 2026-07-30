// Package linereader reads newline-delimited records from an io.Reader,
// truncating any single line that exceeds maxBytes. A truncated line is
// returned with ok=true and truncated=true; the reader consumes through the
// next '\n' so the next ReadLine is aligned. This prevents a single
// pathological line (e.g. a multi-MB tool output) from terminating the
// entire stream with bufio.ErrTooLong.
package linereader

import (
	"bufio"
	"io"

	"github.com/justphantom/lark-bridge/internal/strutil"
)

// LineReader reads newline-delimited records, truncating any single line
// that exceeds maxBytes. A truncated line is returned with truncated=true;
// the reader consumes through the next '\n' so the next Read is aligned.
type LineReader struct {
	r   *bufio.Reader
	max int
}

// New creates a LineReader that reads from r, truncating lines longer than
// maxBytes. maxBytes must be > 0 or reads will always return empty strings.
func New(r io.Reader, maxBytes int) *LineReader {
	return &LineReader{
		r:   bufio.NewReader(r),
		max: maxBytes,
	}
}

// ReadLine reads one line (without the trailing '\n' or '\r\n').
// Returns io.EOF when there is no more data. truncated=true means the line
// was longer than maxBytes and was truncated; the remainder of the line has
// been consumed so the next ReadLine starts at the next record.
//
// The truncated line is the prefix of the original line truncated to maxBytes
// with a "..." suffix appended, so the total length is maxBytes+3.
//
// Memory is bounded by maxBytes plus one bufio buffer (4 KiB): the line is
// accumulated chunk by chunk via ReadSlice, and once the accumulated length
// exceeds maxBytes the rest of the line is discarded without buffering. A
// pathological gigabyte-long line therefore cannot exhaust memory.
func (l *LineReader) ReadLine() (line string, truncated bool, err error) {
	if l.max <= 0 {
		return "", false, io.ErrUnexpectedEOF
	}

	var buf []byte
	for {
		chunk, readErr := l.r.ReadSlice('\n')

		complete := readErr == nil || readErr == io.EOF
		if complete {
			content := chunk
			if readErr == nil && len(content) > 0 && content[len(content)-1] == '\n' {
				content = content[:len(content)-1]
				if len(content) > 0 && content[len(content)-1] == '\r' {
					content = content[:len(content)-1]
				}
			}
			if len(buf)+len(content) > l.max {
				// Line complete but over the cap: truncate, nothing to discard.
				buf = append(buf, content...)
				return strutil.Truncate(string(buf), l.max), true, nil
			}
			buf = append(buf, content...)
			if len(buf) == 0 && readErr == io.EOF {
				return "", false, io.EOF
			}
			return string(buf), false, nil
		}

		if readErr == bufio.ErrBufferFull {
			// Line continues past this chunk (no '\n' within one buffer).
			if len(buf)+len(chunk) > l.max {
				// Cap reached mid-line: keep the first max bytes, drop the
				// rest of the line without buffering it.
				keep := l.max - len(buf)
				if keep > 0 {
					buf = append(buf, chunk[:keep]...)
				}
				l.discardLine()
				// buf is exactly max bytes here, which Truncate would
				// return verbatim (len <= n); the extra byte forces the
				// rune-safe cut + "..." suffix, matching the single-chunk
				// truncation path.
				return strutil.Truncate(string(buf)+"\x00", l.max), true, nil
			}
			buf = append(buf, chunk...)
			continue
		}

		// Real error (e.g. closed pipe). Return what we have.
		if len(buf)+len(chunk) > 0 {
			buf = append(buf, chunk...)
			return stripNewline(string(buf)), false, nil
		}
		return "", false, readErr
	}
}

// stripNewline removes trailing \n or \r\n from s.
func stripNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		s = s[:len(s)-1]
		if len(s) > 0 && s[len(s)-1] == '\r' {
			s = s[:len(s)-1]
		}
	}
	return s
}

// discardLine reads and discards chunks until a newline is found or EOF.
// Uses ReadSlice so the discarded bytes are never accumulated: the reader is
// positioned at the start of the next record with O(1) extra memory.
func (l *LineReader) discardLine() {
	for {
		_, err := l.r.ReadSlice('\n')
		if err != bufio.ErrBufferFull {
			return
		}
	}
}
