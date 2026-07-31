package ws

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestStart_BudgetExhaustedReturnsError pins the W1 fix.
//
// Before the fix, when reconnectStep returned false and ctx was NOT cancelled
// (budget exhausted by a real outage), Start returned ctx.Err() — which is nil
// for an uncancelled ctx — so the bot silently died with no error signal for
// a supervisor (systemd Restart=on-failure) to act on. The fix returns
// ErrReconnectBudgetExhausted instead.
//
// This wires a bootstrap that points at a dead WS URL so the first connect's
// dial fails, then a ReconnectCount=0 budget exhausts on the very first
// reconnect attempt — without cancelling ctx — and asserts Start returns
// ErrReconnectBudgetExhausted (errors.Is), not nil.
func TestStart_BudgetExhaustedReturnsError(t *testing.T) {
	// Bootstrap returns a WS URL to a port nothing listens on → dial fails.
	bsrv := bootstrapServer(t, "ws://127.0.0.1:1/nonexistent", clientConfig{
		ReconnectCount: 0, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 600 * time.Second,
	})
	defer bsrv.Close()

	wc := newWSClient("a", "s", bsrv.URL, bsrv.Client(), Lifecycle{}, &recordingHandler{})
	wc.cfg = clientConfig{ReconnectCount: 0, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 600 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := wc.Start(ctx)
	if err == nil {
		t.Fatal("Start returned nil on reconnect-budget exhaustion (W1 regression); want ErrReconnectBudgetExhausted")
	}
	if !errors.Is(err, ErrReconnectBudgetExhausted) {
		t.Fatalf("Start error = %v, want errors.Is ErrReconnectBudgetExhausted", err)
	}
	// And ctx must NOT have been cancelled (this is the budget path, not Stop).
	if ctx.Err() != nil {
		t.Fatalf("ctx unexpectedly cancelled; budget path should leave ctx alive, got %v", ctx.Err())
	}
}

// TestStart_FiresOnReconnected verifies OnReconnected fires exactly once per
// recovery, and NOT on the initial connect. Drives a real fakeServer, breaks
// the conn so runSession returns, lets one reconnect succeed, then cancels.
func TestStart_FiresOnReconnected(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.Close()
	bsrv := bootstrapServer(t, fs.URL(), clientConfig{
		ReconnectCount: -1, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 600 * time.Second,
	})
	defer bsrv.Close()

	var ready, reconnected atomic.Int32
	wc := newWSClient("a", "s", bsrv.URL, bsrv.Client(), Lifecycle{
		OnReady:       func() { ready.Add(1) },
		OnReconnected: func() { reconnected.Add(1) },
	}, &recordingHandler{})
	wc.cfg = clientConfig{ReconnectCount: -1, ReconnectInterval: 0, ReconnectNonce: 0, PingInterval: 600 * time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- wc.Start(ctx) }()

	// Wait for the initial connect (OnReady), with OnReconnected still at 0.
	if !waitUntil(func() bool { return ready.Load() >= 1 }, 3*time.Second) {
		t.Fatal("OnReady never fired on initial connect")
	}
	if got := reconnected.Load(); got != 0 {
		t.Fatalf("OnReconnected fired %d times on initial connect; want 0", got)
	}

	// Wait for the fake server to have accepted the conn, then drop it — this
	// ends runSession and lets Start reconnect (the budget is infinite).
	if !waitUntil(func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.conns) >= 1
	}, 2*time.Second) {
		t.Fatal("fake server never accepted the initial client conn")
	}
	fs.mu.Lock()
	fs.conns[0].Close()
	fs.mu.Unlock()

	// OnReady fires again on the reconnect; OnReconnected must fire exactly
	// once for that recovery. The dialer reconnects to the same fakeServer,
	// which accepts a fresh conn (its accept loop is still running).
	if !waitUntil(func() bool { return reconnected.Load() >= 1 }, 3*time.Second) {
		t.Fatalf("OnReconnected never fired after reconnect; ready=%d reconnected=%d", ready.Load(), reconnected.Load())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after ctx cancel")
	}
}

// TestFireReconnected_NilSafe is a tiny unit guard: fireReconnected must be
// a no-op when OnReconnected is unset (defensive, mirrors the other fire*).
func TestFireReconnected_NilSafe(t *testing.T) {
	wc := newWSClient("a", "s", "", &http.Client{}, Lifecycle{}, nil)
	// Must not panic with a nil callback.
	wc.fireReconnected()
}
