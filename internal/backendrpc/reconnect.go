package backendrpc

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// reconnectBackoff is the delay before the first reconnect attempt; each
// subsequent failure doubles the delay, capped at reconnectMaxBackoff. Each
// wait is padded with symmetric jitter of ±reconnectJitter×backoff. These
// tunables are package vars (not consts) so tests can shrink them to keep
// give-up scenarios fast; production code never mutates them.
var (
	reconnectBackoff    = 5 * time.Second
	reconnectMaxBackoff = 60 * time.Second
	// maxReconnectFailures caps consecutive reconnect failures before Run
	// returns ErrGiveUpReconnect. With the defaults above the cap is
	// reached after ~15-20min of total frontend outage — long enough to
	// ride out blips, short enough that a permanently-broken frontend
	// restarts the process so the supervisor (systemd Restart=on-failure)
	// can re-resolve DNS / re-read config instead of looping forever.
	maxReconnectFailures = 20
)

const (
	// reconnectJitter is the half-width of the symmetric wait window used
	// by jitteredBackoff, as a fraction of the current backoff. The actual
	// wait lies in [backoff*(1-jitter), backoff*(1+jitter)], so concurrent
	// disconnected backends don't retry in lockstep and a single retry
	// can't run meaningfully earlier than the floor.
	reconnectJitter = 0.5
)

// ErrGiveUpReconnect is returned by Run when Connect has failed
// maxReconnectFailures times in a row without a single successful receive in
// between. Callers MUST treat it as fatal: the process should exit so the
// supervisor (e.g. systemd Restart=on-failure) can restart it from a clean
// state — re-resolving frontend DNS, re-reading config, recovering from an
// outage too long to ride out. It must never be silently swallowed and
// retried in-process.
var ErrGiveUpReconnect = errors.New("backendrpc: give up reconnecting after sustained failures")

// Run connects to the frontend, drains Events via handle, and reconnects with
// exponential backoff when the SSE stream ends for any reason other than ctx
// cancellation. It blocks until ctx is cancelled or an initial Connect fails
// (so a misconfigured frontend_url fails fast at startup).
//
// handle is invoked for every Event received on a live stream. Its error is
// logged and does not terminate the loop — a handler bug should not take the
// backend offline. The EventErr callback, when non-nil, is notified of every
// connect/recv failure and successful reconnect (for observability).
func Run(ctx context.Context, backendID, backendType, frontendURL, secret string,
	handle func(context.Context, *protocol.Event) error,
	eventErr func(err error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Initial connect must succeed; a bad config should fail fast.
	client, err := Connect(backendID, backendType, frontendURL, secret)
	if err != nil {
		return err
	}
	// current holds the live client behind an atomic so the shutdown goroutine
	// can Close it (to unblock RecvEvent) without racing the reconnect loop's
	// reassignment. RecvEvent does not observe ctx, so a ctx cancel can only
	// break it by closing the client.
	var current atomic.Pointer[Client]
	current.Store(client)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			if c := current.Load(); c != nil {
				_ = c.Close() // shutdown path
			}
		case <-stop:
		}
	}()
	backoff := reconnectBackoff
	// failures counts consecutive reconnect attempts without a successful
	// receive in between. It is the gauge for ErrGiveUpReconnect: any
	// delivered event resets it (the stream was proven healthy), so only
	// SUSTAINED outage trips the limit — not isolated blips spread over a
	// long-running session.
	var failures int
	for {
		ev, rerr := client.RecvEvent()
		if rerr != nil {
			_ = client.Close() // conn is broken (RecvEvent errored)
			if ctx.Err() != nil {
				return nil
			}
			if eventErr != nil {
				eventErr(fmt.Errorf("sse recv: %w", rerr))
			}
			// Reconnect with backoff, interruptible by ctx.
			client, err = reconnect(ctx, backendID, backendType, frontendURL, secret, &backoff, &failures, eventErr)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if eventErr != nil {
					eventErr(fmt.Errorf("give up after retries: %w", err))
				}
				return err
			}
			// reconnect may have succeeded during a shutdown: by the time it
			// returns, the ctx-cancel goroutine above has already closed the
			// OLD client and exited, so nothing would ever close this NEW one
			// and RecvEvent would block forever. Close it ourselves and return.
			if ctx.Err() != nil {
				_ = client.Close() // shutdown path
				return nil
			}
			current.Store(client)
			continue
		}
		// Reset backoff and failures after a successful receive: the stream
		// is healthy, so a future outage starts the count from scratch.
		backoff = reconnectBackoff
		failures = 0
		if err := handle(ctx, ev); err != nil {
			if eventErr != nil {
				eventErr(fmt.Errorf("handle: %w", err))
			}
		}
	}
}

// reconnect retries Connect with exponential backoff until success, ctx
// cancellation, or maxReconnectFailures consecutive failures. *failures is
// incremented on every attempt and is NOT reset here on Connect success —
// only Run's receive-success path resets it, so a server that handshakes
// then immediately drops the stream still drives the count toward the
// give-up threshold. Returns ErrGiveUpReconnect (wrap-aware via errors.Is)
// when the threshold is reached.
func reconnect(ctx context.Context, backendID, backendType, frontendURL, secret string,
	backoff *time.Duration, failures *int, eventErr func(error)) (*Client, error) {
	for {
		*failures++
		if *failures >= maxReconnectFailures {
			return nil, fmt.Errorf("%w: %d consecutive attempts", ErrGiveUpReconnect, *failures)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(jitteredBackoff(*backoff)):
		}
		c, err := Connect(backendID, backendType, frontendURL, secret)
		if err == nil {
			if eventErr != nil {
				eventErr(errors.New("reconnected"))
			}
			// Do NOT reset backoff or failures here: a server that accepts
			// the SSE handshake and then immediately closes the stream
			// would pin both at the floor forever, producing a tight
			// connect/drop storm that never gives up. Reset belongs in Run,
			// gated on a successful receive — the only proof the stream is
			// actually healthy.
			return c, nil
		}
		if eventErr != nil {
			eventErr(fmt.Errorf("reconnect: %w", err))
		}
		*backoff *= 2
		if *backoff > reconnectMaxBackoff {
			*backoff = reconnectMaxBackoff
		}
	}
}

// jitteredBackoff returns a uniformly random wait in
// [d*(1-reconnectJitter), d*(1+reconnectJitter)], decoupling concurrent
// backends' retry cadence. The window is symmetric around d so a wave of
// simultaneous disconnects spreads both earlier and later than the floor.
func jitteredBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	lo := int64(float64(d) * (1 - reconnectJitter))
	hi := int64(float64(d) * (1 + reconnectJitter))
	return time.Duration(lo + rand.Int64N(hi-lo+1)) //nolint:gosec // G404: 重连退避抖动仅为打散同步重连，非密码学场景
}
