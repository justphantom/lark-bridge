package backendrpc

import (
	"context"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestRun_InitialConnectFails verifies Run returns the connect error
// immediately when the frontend is unreachable (fail-fast on bad config).
func TestRun_InitialConnectFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Run(ctx, "b1", "claude", "http://127.0.0.1:0", "",
		func(context.Context, *protocol.Event) error { return nil },
		func(error) {})
	if err == nil {
		t.Fatal("expected connect error, got nil")
	}
}

// TestRun_ReceivesEventsThenExitsOnCancel verifies the happy path: Run
// connects, delivers Events to handle, and returns nil when ctx is cancelled.
func TestRun_ReceivesEventsThenExitsOnCancel(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var got atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, "back-run", "claude", ts.URL, "",
			func(_ context.Context, ev *protocol.Event) error {
				if ev.Type == protocol.TypePing {
					got.Add(1)
				}
				return nil
			},
			func(error) {})
	}()

	// Wait for the backend to register (handleSSE registers after the SSE
	// handshake flushes, so Connect returning does not guarantee registration).
	regReady := time.After(2 * time.Second)
	for {
		if _, ok := reg.Get("back-run"); ok {
			break
		}
		select {
		case <-regReady:
			t.Fatal("backend never registered")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := reg.SendEvent("back-run", &protocol.Event{Type: protocol.TypePing, Ping: &protocol.PingPayload{}}); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	// Expect the event to be delivered.
	deadline := time.After(2 * time.Second)
	for got.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for event delivery")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRun_ReconnectsAfterStreamEnd verifies that when the SSE stream ends
// mid-run (frontend closes the connection), Run reconnects and continues
// delivering events. Exercises the exponential-backoff path once.
func TestRun_ReconnectsAfterStreamEnd(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got atomic.Int32
	var events []string
	var mu sync.Mutex
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, "back-rc", "claude", ts.URL, "",
			func(_ context.Context, ev *protocol.Event) error {
				if ev.Type == protocol.TypePing {
					mu.Lock()
					events = append(events, ev.PromptID)
					mu.Unlock()
					got.Add(1)
				}
				return nil
			},
			func(error) {})
	}()

	// Wait for registration + first event.
	regReady := time.After(2 * time.Second)
	for {
		if _, ok := reg.Get("back-rc"); ok {
			break
		}
		select {
		case <-regReady:
			t.Fatal("backend never registered")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	_ = reg.SendEvent("back-rc", &protocol.Event{Type: protocol.TypePing, PromptID: "first", Ping: &protocol.PingPayload{}})
	for got.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	// Kill the SSE connection by re-registering the backend (Register closes
	// the old conn's eventCh, which ends the old readSSE goroutine → RecvEvent
	// errors → Run enters its reconnect path).
	_ = reg.SendEvent("back-rc", &protocol.Event{Type: protocol.TypePing, PromptID: "dropped", Ping: &protocol.PingPayload{}})
	// Force-close the conn so the stream definitively ends.
	if c, ok := reg.Get("back-rc"); ok {
		// Unregister to close the conn; Run will reconnect and re-register.
		reg.Unregister("back-rc")
		_ = c
	}

	// After reconnect (backoff starts at reconnectBackoff, jittered up to
	// backoff*(1+reconnectJitter)), send a second event. Give the reconnect
	// one round — wait past the jittered window first.
	maxFirstWait := time.Duration(float64(reconnectBackoff) * (1 + reconnectJitter))
	go func() {
		time.Sleep(maxFirstWait + 500*time.Millisecond)
		_ = reg.SendEvent("back-rc", &protocol.Event{Type: protocol.TypePing, PromptID: "second", Ping: &protocol.PingPayload{}})
	}()

	deadline := time.After(maxFirstWait + 5*time.Second)
	for got.Load() < 2 {
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timeout: received events %v", events)
		default:
			time.Sleep(20 * time.Millisecond)
		}
	}
	cancel()
	ts.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestJitteredBackoff_Range verifies the wait always lies in
// [backoff, backoff*(1+reconnectJitter)] and that jitter is actually applied
// (not a constant floor).
func TestJitteredBackoff_Range(t *testing.T) {
	d := reconnectBackoff
	hi := time.Duration(float64(d) * (1 + reconnectJitter))
	var jitteredSeen bool
	for range 1000 {
		got := jitteredBackoff(d)
		if got < d || got > hi {
			t.Fatalf("jitteredBackoff(%v) = %v, want [%v, %v]", d, got, d, hi)
		}
		if got > d {
			jitteredSeen = true
		}
	}
	if !jitteredSeen {
		t.Fatal("jitteredBackoff never added slack across 1000 samples; jitter not applied")
	}
	if got := jitteredBackoff(0); got != 0 {
		t.Fatalf("jitteredBackoff(0) = %v, want 0", got)
	}
}
