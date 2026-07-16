package peribridge

import (
	"strings"
	"time"
)

// textThrottle batches per-token text deltas and releases them at most once
// per interval. peri's stream-json splits even short replies into one-char
// chunks (observed: 125 events for 230 chars), so emitting each delta as its
// own Control floods the IPC channel and starves the frontend render loop.
//
// The throttle accumulates chunks in a buffer; Add returns the flushed batch
// when the interval has elapsed since the last flush, or "" when the window
// has not elapsed (caller drops it — the bytes are retained for the next
// window). Flush forces a drain for the terminal path so trailing bytes are
// not lost.
type textThrottle struct {
	interval time.Duration
	last     time.Time
	buf      strings.Builder
}

func newTextThrottle(interval time.Duration) *textThrottle {
	return &textThrottle{interval: interval}
}

// Add appends a text chunk to the pending batch. It returns the concatenated
// batch to emit when the interval has elapsed since the last flush (and resets
// the buffer); otherwise it returns "" and the chunk stays buffered. A zero or
// negative interval disables throttling — every Add returns its chunk
// immediately so the bridge still emits, just unbatched.
func (t *textThrottle) Add(chunk string, now time.Time) string {
	if t.interval <= 0 {
		return chunk
	}
	t.buf.WriteString(chunk)
	if t.last.IsZero() || now.Sub(t.last) >= t.interval {
		out := t.buf.String()
		t.buf.Reset()
		t.last = now
		return out
	}
	return ""
}

// Flush returns any buffered text and clears it. Called at terminal/cancel so
// the final partial batch is not lost. Returns "" when nothing is pending.
func (t *textThrottle) Flush() string {
	out := t.buf.String()
	t.buf.Reset()
	if out != "" {
		t.last = time.Now()
	}
	return out
}
