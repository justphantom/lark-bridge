package bridgebase

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// PeriodicReporter runs a Tick function on a fixed interval until ctx is
// cancelled. The first tick fires immediately (so a freshly-started backend
// surfaces without waiting one interval), subsequent ones fire on the
// ticker.
//
// status-monitor's Run loop is the canonical caller; the abstraction lets a
// future periodic backend (or a test) drive the same shape without
// re-implementing the ticker + immediate-first-tick dance.
type PeriodicReporter struct {
	interval time.Duration
	logger   *log.Logger
	tick     func(ctx context.Context)
	now      func() time.Time
}

// NewPeriodicReporter builds a reporter. interval <=0 defaults to 60s. A nil
// tick is a no-op (caller-side misconfiguration that surfaces as "the card
// never updates" rather than a panic). A nil logger is replaced with no-op.
func NewPeriodicReporter(interval time.Duration, logger *log.Logger, tick func(ctx context.Context)) *PeriodicReporter {
	if logger == nil {
		logger = log.Nop()
	}
	if interval <= 0 {
		interval = 60 * time.Second
	}
	if tick == nil {
		tick = func(context.Context) {}
	}
	return &PeriodicReporter{interval: interval, logger: logger, tick: tick, now: time.Now}
}

// Run ticks once immediately, then every interval until ctx is cancelled.
// Call on its own goroutine so the SSE event loop is never blocked.
func (r *PeriodicReporter) Run(ctx context.Context) error {
	r.tick(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.tick(ctx)
		}
	}
}
