package opencodebridge

import (
	"testing"
	"time"
)

// TestShouldEmitText_FirstCallTrue verifies the first call always emits,
// regardless of interval.
func TestShouldEmitText_FirstCallTrue(t *testing.T) {
	th := newControlThrottle(200 * time.Millisecond)
	if !th.shouldEmitText(time.Now()) {
		t.Error("first call should always emit")
	}
}

// TestShouldEmitText_ThrottlesWithinInterval verifies calls within the interval
// after an emit are suppressed.
func TestShouldEmitText_ThrottlesWithinInterval(t *testing.T) {
	th := newControlThrottle(200 * time.Millisecond)
	base := time.Now()
	if !th.shouldEmitText(base) {
		t.Fatal("first call should emit")
	}
	// Subsequent calls within the interval must be suppressed.
	for _, offset := range []time.Duration{10, 50, 100, 199} {
		if th.shouldEmitText(base.Add(offset * time.Millisecond)) {
			t.Errorf("call at +%dms should be throttled", offset)
		}
	}
}

// TestShouldEmitText_EmitsAfterInterval verifies a call at or beyond the
// interval emits and resets the window.
func TestShouldEmitText_EmitsAfterInterval(t *testing.T) {
	th := newControlThrottle(200 * time.Millisecond)
	base := time.Now()
	th.shouldEmitText(base)
	// Exactly at interval boundary.
	if !th.shouldEmitText(base.Add(200 * time.Millisecond)) {
		t.Error("call at interval boundary should emit")
	}
	// After the emit, the next call within the new window is throttled.
	if th.shouldEmitText(base.Add(250 * time.Millisecond)) {
		t.Error("call 50ms after re-emit should be throttled")
	}
}

// TestShouldEmitText_ZeroIntervalAlwaysEmits verifies interval<=0 disables
// throttling (every call emits).
func TestShouldEmitText_ZeroIntervalAlwaysEmits(t *testing.T) {
	th := newControlThrottle(0)
	now := time.Now()
	for i := 0; i < 5; i++ {
		if !th.shouldEmitText(now.Add(time.Duration(i) * time.Microsecond)) {
			t.Errorf("call %d: interval<=0 should always emit", i)
		}
	}
}

// TestShouldEmitText_ConcurrentSafe verifies the throttle does not panic under
// concurrent access (race detector enforces this when run with -race).
func TestShouldEmitText_ConcurrentSafe(t *testing.T) {
	th := newControlThrottle(time.Millisecond)
	done := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				th.shouldEmitText(time.Now())
			}
		}()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
}
