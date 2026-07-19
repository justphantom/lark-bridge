package bridgebase

import (
	"strings"
	"testing"
	"time"
)

// TestShouldEmitText_FirstCallTrue verifies the first call always emits,
// regardless of interval.
func TestShouldEmitText_FirstCallTrue(t *testing.T) {
	th := NewControlThrottle(200 * time.Millisecond)
	if !th.ShouldEmitText(time.Now()) {
		t.Error("first call should always emit")
	}
}

// TestShouldEmitText_ThrottlesWithinInterval verifies calls within the interval
// after an emit are suppressed.
func TestShouldEmitText_ThrottlesWithinInterval(t *testing.T) {
	th := NewControlThrottle(200 * time.Millisecond)
	base := time.Now()
	if !th.ShouldEmitText(base) {
		t.Fatal("first call should emit")
	}
		// Subsequent calls within the interval must be suppressed.
		for _, offsetMS := range []int{10, 50, 100, 199} {
			offset := time.Duration(offsetMS) * time.Millisecond
			if th.ShouldEmitText(base.Add(offset)) {
				t.Errorf("call at +%dms should be throttled", offsetMS)
		}
	}
}

// TestShouldEmitText_EmitsAfterInterval verifies a call at or beyond the
// interval emits and resets the window.
func TestShouldEmitText_EmitsAfterInterval(t *testing.T) {
	th := NewControlThrottle(200 * time.Millisecond)
	base := time.Now()
	th.ShouldEmitText(base)
	// Exactly at interval boundary.
	if !th.ShouldEmitText(base.Add(200 * time.Millisecond)) {
		t.Error("call at interval boundary should emit")
	}
	// After the emit, the next call within the new window is throttled.
	if th.ShouldEmitText(base.Add(250 * time.Millisecond)) {
		t.Error("call 50ms after re-emit should be throttled")
	}
}

// TestShouldEmitText_ZeroIntervalAlwaysEmits verifies interval<=0 disables
// throttling (every call emits).
func TestShouldEmitText_ZeroIntervalAlwaysEmits(t *testing.T) {
	th := NewControlThrottle(0)
	now := time.Now()
	for i := range 5 {
		if !th.ShouldEmitText(now.Add(time.Duration(i) * time.Microsecond)) {
			t.Errorf("call %d: interval<=0 should always emit", i)
		}
	}
}

// TestShouldEmitText_ConcurrentSafe verifies the throttle does not panic under
// concurrent access (race detector enforces this when run with -race).
func TestShouldEmitText_ConcurrentSafe(t *testing.T) {
	th := NewControlThrottle(time.Millisecond)
	done := make(chan struct{}, 4)
	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				th.ShouldEmitText(time.Now())
			}
		}()
	}
	for range 4 {
		<-done
	}
}

// TestTextThrottle_DisabledWhenNoInterval verifies a zero/negative interval
// passes every chunk through immediately (no batching) so the bridge still
// emits when throttling is off.
func TestTextThrottle_DisabledWhenNoInterval(t *testing.T) {
	for _, interval := range []time.Duration{0, -1} {
		th := NewTextThrottle(interval)
		if got := th.Add("a", time.Now()); got != "a" {
			t.Errorf("interval=%v: Add = %q, want %q", interval, got, "a")
		}
		if got := th.Add("b", time.Now()); got != "b" {
			t.Errorf("interval=%v: Add = %q, want %q", interval, got, "b")
		}
	}
}

// TestTextThrottle_BatchesWithinWindow verifies chunks accumulate and are
// released as one merged delta only after the interval elapses.
func TestTextThrottle_BatchesWithinWindow(t *testing.T) {
	th := NewTextThrottle(100 * time.Millisecond)
	base := time.Now()

	// First Add primes last and returns immediately (first-chunk flush).
	if got := th.Add("h", base); got != "h" {
		t.Fatalf("first Add = %q, want %q", got, "h")
	}
	// Within the window: accumulate, return "".
	for _, c := range []string{"e", "l", "l", "o"} {
		if got := th.Add(c, base.Add(10*time.Millisecond)); got != "" {
			t.Fatalf("in-window Add %q = %q, want empty", c, got)
		}
	}
	// After the window: merged batch released.
	got := th.Add("!", base.Add(101*time.Millisecond))
	if got != "ello!" {
		t.Errorf("post-window Add = %q, want %q", got, "ello!")
	}
}

// TestTextThrottle_Flush verifies Flush drains any buffered remainder; critical
// for the terminal path so trailing bytes are not lost.
func TestTextThrottle_Flush(t *testing.T) {
	th := NewTextThrottle(100 * time.Millisecond)
	base := time.Now()
	th.Add("a", base)                          // primes last, buffer now empty
	th.Add("bc", base.Add(1*time.Millisecond)) // buffered
	th.Add("de", base.Add(2*time.Millisecond)) // buffered

	got := th.Flush()
	if got != "bcde" {
		t.Errorf("Flush = %q, want %q", got, "bcde")
	}
	// Second Flush is a no-op.
	if got := th.Flush(); got != "" {
		t.Errorf("second Flush = %q, want empty", got)
	}
}

// TestTextThrottle_FlushEmpty verifies Flush on a never-used throttle returns
// "" without setting last (so a subsequent Add still flushes on first call).
func TestTextThrottle_FlushEmpty(t *testing.T) {
	th := NewTextThrottle(100 * time.Millisecond)
	if got := th.Flush(); got != "" {
		t.Errorf("Flush on empty = %q, want empty", got)
	}
	if got := th.Add("x", time.Now()); got != "x" {
		t.Errorf("Add after empty Flush = %q, want %q", got, "x")
	}
}

// TestTextThrottle_PreservesAllBytes is a regression guard: across a long
// fragmented run (one-char-per-event pattern) the concatenation of every Add
// return + final Flush must equal the input.
func TestTextThrottle_PreservesAllBytes(t *testing.T) {
	th := NewTextThrottle(50 * time.Millisecond)
	input := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 10) // 260 chars
	var emitted strings.Builder
	start := time.Now()
	for i, r := range input {
		// Advance time slowly so several chunks batch per window.
		if delta := th.Add(string(r), start.Add(time.Duration(i)*time.Millisecond)); delta != "" {
			emitted.WriteString(delta)
		}
	}
	emitted.WriteString(th.Flush())
	if emitted.String() != input {
		t.Errorf("round-trip lost bytes: got %d bytes, want %d", emitted.Len(), len(input))
	}
}
