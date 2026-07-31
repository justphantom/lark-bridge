package ws

import (
	"context"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/lark/websocket"
)

// TestRunSession_ReturnsAfterConnBreak pins the R1 deadlock fix.
//
// Before the fix, runSession deadlocked on a NON-ctx connection break:
// receiveLoop surfaced the read error and closed firstExit; conn.Close()
// unblocked pingLoop; but the sweep goroutine selects ONLY on ctx.Done()
// (it never touches conn), so it stayed parked forever waiting on a ctx
// the caller never cancelled — and <-exitCh (the wg.Wait() gate) blocked
// forever in turn. The explicit cancel() between conn.Close() and
// <-exitCh breaks the cycle.
//
// This reproduces the trigger (server-side conn drop, ctx left alive) and
// asserts runSession returns promptly. A pre-fix build hangs here and
// fails on the select timeout; the fix makes the done channel win.
func TestRunSession_ReturnsAfterConnBreak(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.Close()

	// Dial the fake server directly to obtain a live *websocket.Conn; the
	// fake's handle() performs the RFC 6455 upgrade handshake.
	conn, resp, err := websocket.Dial(context.Background(), fs.URL(), nil)
	if err != nil {
		t.Fatalf("dial fake server: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	defer func() { _ = conn.Close() }()

	wc := newWSClient("a", "s", "", &http.Client{}, Lifecycle{}, &recordingHandler{})
	// Long PingInterval so neither pingLoop ticks nor the 2× read deadline
	// fires during the test; the only exit trigger is our server-side close.
	wc.cfg = clientConfig{PingInterval: 600 * time.Second, ReconnectCount: -1}

	// A deliberately non-cancelled ctx: the bug is that runSession relied on
	// ctx cancellation to release the sweep goroutine when the conn broke.
	// A generous outer timeout lets the select below fail fast on a
	// regression instead of hanging the whole suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { wc.runSession(ctx, conn); close(done) }()

	// Wait for the fake server to have accepted our conn, then drop the
	// server side — this surfaces as a read error in receiveLoop and a write
	// error in pingLoop, exactly the non-ctx break the bug class covers.
	if !waitUntil(func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.conns) >= 1
	}, 2*time.Second) {
		t.Fatal("fake server never accepted the client conn")
	}
	fs.mu.Lock()
	serverConn := fs.conns[0]
	fs.mu.Unlock()
	_ = serverConn.Close()

	select {
	case <-done:
		// runSession returned without ctx cancellation — the fix worked.
	case <-time.After(2 * time.Second):
		t.Fatal("runSession did not return after conn break (R1 deadlock regression)")
	}
}

// TestRunSession_NoGoroutineLeakAfterConnBreak asserts all three session
// loops (receive / ping / sweep) return on a conn break, not just two of
// them. Pre-fix, the sweep goroutine leaked per reconnect.
func TestRunSession_NoGoroutineLeakAfterConnBreak(t *testing.T) {
	// Let prior-test goroutines settle, then snapshot the baseline before
	// we start the session goroutines.
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()

	fs := newFakeServer(t)
	defer fs.Close()

	conn, resp, err := websocket.Dial(context.Background(), fs.URL(), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil {
		resp.Body.Close()
	}
	defer func() { _ = conn.Close() }()

	wc := newWSClient("a", "s", "", &http.Client{}, Lifecycle{}, &recordingHandler{})
	wc.cfg = clientConfig{PingInterval: 600 * time.Second, ReconnectCount: -1}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { wc.runSession(ctx, conn); close(done) }()

	if !waitUntil(func() bool {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return len(fs.conns) >= 1
	}, 2*time.Second) {
		t.Fatal("fake server never accepted the client conn")
	}
	fs.mu.Lock()
	serverConn := fs.conns[0]
	fs.mu.Unlock()
	_ = serverConn.Close()

	// Close the fake server BEFORE counting goroutines so its accept/handle
	// helpers exit — otherwise they sit in Accept/Read for the deferred
	// fs.Close and the delta is meaningless. The three runSession loops
	// (receive/ping/sweep) already returned by this point (done fired),
	// which is itself the structural guarantee: runSession cannot return
	// until its internal wg.Wait() completes.
	_ = fs.Close()

	// Settle: let the runtime reclaim the just-exited goroutines + sweep
	// ticker goroutine (the ticker's defer-Stop already ran).
	time.Sleep(150 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if leaked := after - before; leaked > 0 {
		t.Fatalf("goroutine leak after runSession (before=%d after=%d, leaked=%d)", before, after, leaked)
	}
}
