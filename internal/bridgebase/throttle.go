package bridgebase

import (
	"strings"
	"sync"
	"time"
)

// ControlThrottle limits how often streaming text deltas are
// forwarded to the frontend as Control messages, so a fast-typing model does
// not flood the IPC channel (and the frontend's UpdateCard path). Tool,
// result, and error controls bypass the throttle and are always sent
// immediately. Safe for concurrent use.
type ControlThrottle struct {
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

// NewControlThrottle builds a throttle that emits at most one text
// control per interval.
func NewControlThrottle(interval time.Duration) *ControlThrottle {
	return &ControlThrottle{interval: interval}
}

// ShouldEmitText reports whether a TypeText control should be
// emitted now. Returns true on the first call and at most once per interval
// thereafter. A zero or negative interval disables throttling.
func (t *ControlThrottle) ShouldEmitText(now time.Time) bool {
	if t.interval <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last.IsZero() || now.Sub(t.last) >= t.interval {
		t.last = now
		return true
	}
	return false
}

// TextThrottle batches per-token text deltas and releases them at most once
// per interval. Use this instead of ControlThrottle for backends whose stream
// splits even short replies into tiny chunks (observed on peri: 125 events
// for 230 chars) — dropping deltas ControlThrottle-style would discard text,
// so chunks are accumulated instead.
//
// Add returns the flushed batch when the interval has elapsed since the last
// flush, or "" when the window has not elapsed (the bytes are retained for
// the next window). Flush forces a drain for the terminal path so trailing
// bytes are not lost. NOT safe for concurrent use: the stream loop that owns
// it is single-goroutine by design.
type TextThrottle struct {
	interval time.Duration
	last     time.Time
	buf      strings.Builder
}

// NewTextThrottle builds a batching throttle with the given release interval.
func NewTextThrottle(interval time.Duration) *TextThrottle {
	return &TextThrottle{interval: interval}
}

// Add appends a text chunk to the pending batch. It returns the concatenated
// batch to emit when the interval has elapsed since the last flush (and resets
// the buffer); otherwise it returns "" and the chunk stays buffered. A zero or
// negative interval disables throttling — every Add returns its chunk
// immediately so the bridge still emits, just unbatched.
func (t *TextThrottle) Add(chunk string, now time.Time) string {
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
func (t *TextThrottle) Flush() string {
	out := t.buf.String()
	t.buf.Reset()
	if out != "" {
		t.last = time.Now()
	}
	return out
}
