package ws

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/url"
	"strconv"
	"time"
)

// ErrReconnectBudgetExhausted is returned by Start when the server-supplied
// ReconnectCount budget runs out WITHOUT ctx being cancelled — i.e. a real
// outage the client could not recover from. Returning an explicit error (vs.
// the pre-fix silent nil) lets a supervisor like systemd Restart=on-failure
// restart the process so it re-bootstraps from a clean state.
var ErrReconnectBudgetExhausted = errors.New("ws: reconnect budget exhausted (server-supplied ReconnectCount reached)")

// reconnectSleep blocks for the configured nonce/interval before the next
// reconnect attempt. Returns false if ctx was cancelled during the wait.
func (c *Client) reconnectSleep(ctx context.Context) bool {
	c.mu.Lock()
	nonce := c.cfg.ReconnectNonce
	interval := c.cfg.ReconnectInterval
	c.mu.Unlock()
	c.fireReconnecting()
	if nonce > 0 {
		// math/rand/v2 is intentional: the jitter is a reconnect-backoff
		// spread, not a security primitive — crypto/rand would add cost for
		// no benefit (an attacker observing reconnect timing learns nothing
		// of value from the backoff distribution).
		jitter := time.Duration(rand.Int64N(int64(nonce))) //nolint:gosec // G404: reconnect-backoff jitter, not a security primitive.
		if !sleepCtx(ctx, jitter) {
			return false
		}
	}
	return sleepCtx(ctx, interval)
}

// sleepCtx sleeps for d but aborts early (returning false) when ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// fatalError marks an error as non-retryable (auth/credentials). Start returns
// it immediately instead of looping forever.
type fatalError struct{ error }

func (fatalError) IsFatal() {}

// isFatal reports whether the bootstrap/connect path should stop retrying.
func isFatal(err error) bool {
	var f interface{ IsFatal() }
	return errors.As(err, &f)
}

// fireXXX helpers invoke the lifecycle callback if set. They snapshot lc
// under the lock so a post-Start SetLifecycle is safe to call (the contract
// says "before Start", but we stay race-clean regardless).
func (c *Client) fireReady() {
	lc := c.snapshotLC()
	if lc.OnReady != nil {
		lc.OnReady()
	}
}
func (c *Client) fireError(err error) {
	lc := c.snapshotLC()
	if lc.OnError != nil {
		lc.OnError(err)
	}
}
func (c *Client) fireReconnected() {
	lc := c.snapshotLC()
	if lc.OnReconnected != nil {
		lc.OnReconnected()
	}
}
func (c *Client) fireReconnecting() {
	lc := c.snapshotLC()
	if lc.OnReconnecting != nil {
		lc.OnReconnecting()
	}
}
func (c *Client) fireDisconnected() {
	lc := c.snapshotLC()
	if lc.OnDisconnected != nil {
		lc.OnDisconnected()
	}
}

// snapshotLC returns a value copy of the current lifecycle.
func (c *Client) snapshotLC() Lifecycle {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lc
}

// snapshotSink returns the current sink.
func (c *Client) snapshotSink() Sink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sink
}

// parseServiceID extracts the service_id query param as int32 (seeds pings).
func parseServiceID(rawURL string) int32 {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(u.Query().Get(queryServiceID), 10, 32)
	if err != nil {
		return 0
	}
	return int32(v)
}
