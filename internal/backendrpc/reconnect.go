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
// wait is padded with up to reconnectJitter×backoff of random slack.
const (
	reconnectBackoff    = 5 * time.Second
	reconnectMaxBackoff = 60 * time.Second
	// reconnectJitter is the random fraction of backoff added to each wait so
	// concurrent disconnected backends don't retry in lockstep. The actual wait
	// lies in [backoff, backoff*(1+reconnectJitter)].
	reconnectJitter = 0.5
)

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
			client, err = reconnect(ctx, backendID, backendType, frontendURL, secret, &backoff, eventErr)
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
		// Reset backoff after a successful receive: the stream is healthy.
		backoff = reconnectBackoff
		if err := handle(ctx, ev); err != nil {
			if eventErr != nil {
				eventErr(fmt.Errorf("handle: %w", err))
			}
		}
	}
}

// reconnect retries Connect with exponential backoff until success or ctx
// cancellation. It returns nil only when ctx is done.
func reconnect(ctx context.Context, backendID, backendType, frontendURL, secret string,
	backoff *time.Duration, eventErr func(error)) (*Client, error) {
	for {
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
			// Do NOT reset backoff here: a server that accepts the SSE
			// handshake and then immediately closes the stream would
			// pin backoff at the floor forever, producing a tight
			// connect/drop storm. Reset only happens in Run after a
			// successful receive — that's the only proof the stream is
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

// jitteredBackoff returns d plus up to reconnectJitter×d of uniformly random
// slack, decoupling concurrent backends' retry cadence.
func jitteredBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d + time.Duration(rand.Int64N(int64(float64(d)*reconnectJitter)+1)) //nolint:gosec // G404: 重连退避抖动仅为打散同步重连，非密码学场景
}
