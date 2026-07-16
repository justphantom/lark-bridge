package peribridge

import (
	"sync"
	"time"
)

// controlThrottle limits how often streaming text/thinking deltas are
// forwarded to the frontend as Control messages, so a fast-typing model does
// not flood the IPC channel (and the frontend's UpdateCard path).
// Tool/result/error controls bypass the throttle and are always sent
// immediately.
type controlThrottle struct {
	interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

func newControlThrottle(interval time.Duration) *controlThrottle {
	return &controlThrottle{interval: interval}
}

// shouldEmitText reports whether a TypeText/TypeThinking control should be
// emitted now. Returns true on the first call and at most once per interval
// thereafter.
func (t *controlThrottle) shouldEmitText(now time.Time) bool {
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
