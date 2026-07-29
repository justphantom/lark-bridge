package bridgebase

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// TestPeriodicReporter_FiresImmediatelyThenOnInterval verifies the first
// tick fires without waiting one interval, and subsequent ticks fire on the
// ticker.
func TestPeriodicReporter_FiresImmediatelyThenOnInterval(t *testing.T) {
	var ticks int32
	r := NewPeriodicReporter(20*time.Millisecond, log.Nop(), func(context.Context) {
		atomic.AddInt32(&ticks, 1)
	})

	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	// Wait for first tick (immediate).
	deadline := time.After(time.Second)
	for atomic.LoadInt32(&ticks) == 0 {
		select {
		case <-deadline:
			t.Fatal("first tick never fired")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	first := atomic.LoadInt32(&ticks)
	// Wait for at least one more tick (interval-bound).
	for atomic.LoadInt32(&ticks) == first {
		select {
		case <-deadline:
			t.Fatal("second tick never fired")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	cancel()
}

// TestPeriodicReporter_DefaultsInterval verifies interval<=0 falls back to
// 60s (the canonical status-monitor period).
func TestPeriodicReporter_DefaultsInterval(t *testing.T) {
	r := NewPeriodicReporter(0, nil, nil)
	if r.interval != 60*time.Second {
		t.Errorf("interval = %v, want 60s", r.interval)
	}
}

// TestPeriodicReporter_CancelTerminates verifies ctx cancel ends Run.
func TestPeriodicReporter_CancelTerminates(t *testing.T) {
	r := NewPeriodicReporter(time.Hour, log.Nop(), func(context.Context) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not terminate after cancel")
	}
}
