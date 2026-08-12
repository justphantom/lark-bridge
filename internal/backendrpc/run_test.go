package backendrpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestConnect_UnreachableFails verifies Connect fails fast when the frontend
// is unreachable (bad config / frontend down at startup). RunWithClient
// assumes the caller has already connected, so this fail-fast lives in Connect.
func TestConnect_UnreachableFails(t *testing.T) {
	_, err := Connect(ConnectOptions{BackendID: "b1", BackendType: "claude", FrontendURL: "http://127.0.0.1:0"})
	if err == nil {
		t.Fatal("expected connect error for unreachable frontend, got nil")
	}
}

// TestRun_ReceivesEventsThenExitsOnCancel verifies the happy path: RunWithClient
// connects, delivers Events to handle, and returns nil when ctx is cancelled.
func TestRun_ReceivesEventsThenExitsOnCancel(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var got atomic.Int32
	done := make(chan error, 1)
	opts := ConnectOptions{BackendID: "back-run", BackendType: "claude", FrontendURL: ts.URL}
	client, err := Connect(opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go func() {
		done <- RunWithClient(ctx, client, opts,
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
			t.Fatalf("RunWithClient returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithClient did not return after cancel")
	}
}

// TestRun_ReconnectsAfterStreamEnd verifies that when the SSE stream ends
// mid-run (frontend closes the connection), RunWithClient reconnects and continues
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
	opts := ConnectOptions{BackendID: "back-rc", BackendType: "claude", FrontendURL: ts.URL}
	client, err := Connect(opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go func() {
		done <- RunWithClient(ctx, client, opts,
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
	// errors → RunWithClient enters its reconnect path).
	_ = reg.SendEvent("back-rc", &protocol.Event{Type: protocol.TypePing, PromptID: "dropped", Ping: &protocol.PingPayload{}})
	// Force-close the conn so the stream definitively ends.
	if c, ok := reg.Get("back-rc"); ok {
		// Unregister to close the conn; RunWithClient will reconnect and re-register.
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
		t.Fatal("RunWithClient did not return after cancel")
	}
}

// TestReconnect_NoBackoffResetOnConnectSuccess pins the oscillation-storm
// fix: when Connect succeeds, reconnect must NOT reset *backoff to the
// floor. A server that handshakes then immediately drops the stream would
// otherwise pin backoff at reconnectBackoff forever, producing a tight
// connect/drop storm. Reset belongs in RunWithClient, gated on a successful receive.
func TestReconnect_NoBackoffResetOnConnectSuccess(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pick a backoff != reconnectBackoff so a reset is detectable; keep it
	// small so the test does not spend the backoff window waiting.
	initial := 100 * time.Millisecond
	backoff := initial
	var failures int
	c, err := reconnect(ctx, ConnectOptions{BackendID: "b-reset", BackendType: "claude", FrontendURL: ts.URL}, &backoff, &failures, nil)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer c.Close()
	if backoff != initial {
		t.Fatalf("backoff reset to %v after Connect success, want unchanged %v", backoff, initial)
	}
}

// TestJitteredBackoff_Range verifies the wait always lies in the symmetric
// window [backoff*(1-jitter), backoff*(1+jitter)] and that both halves of
// the window are actually reachable (so a future tightening to
// [backoff, backoff*(1+jitter)] would be caught).
func TestJitteredBackoff_Range(t *testing.T) {
	d := reconnectBackoff
	lo := time.Duration(float64(d) * (1 - reconnectJitter))
	hi := time.Duration(float64(d) * (1 + reconnectJitter))
	var belowFloor, aboveFloor int
	for range 1000 {
		got := jitteredBackoff(d)
		if got < lo || got > hi {
			t.Fatalf("jitteredBackoff(%v) = %v, want [%v, %v]", d, got, lo, hi)
		}
		if got < d {
			belowFloor++
		} else if got > d {
			aboveFloor++
		}
	}
	if belowFloor == 0 {
		t.Fatal("jitteredBackoff never produced a wait below d across 1000 samples; lower half unreachable")
	}
	if aboveFloor == 0 {
		t.Fatal("jitteredBackoff never produced a wait above d across 1000 samples; upper half unreachable")
	}
	if got := jitteredBackoff(0); got != 0 {
		t.Fatalf("jitteredBackoff(0) = %v, want 0", got)
	}
}

// patchReconnectTunables swaps the package-level reconnect tunables for
// fast-testing values and returns a restore func. Tests for the give-up
// path MUST call this: with the production 5s/60s/20 defaults a single
// give-up takes ~15min, which is unacceptable in `go test`.
func patchReconnectTunables(t *testing.T, backoff, maxBackoff time.Duration, maxFailures int) {
	t.Helper()
	origBackoff, origMaxBackoff, origMaxFailures :=
		reconnectBackoff, reconnectMaxBackoff, maxReconnectFailures
	reconnectBackoff, reconnectMaxBackoff, maxReconnectFailures = backoff, maxBackoff, maxFailures
	t.Cleanup(func() {
		reconnectBackoff, reconnectMaxBackoff, maxReconnectFailures =
			origBackoff, origMaxBackoff, origMaxFailures
	})
}

// TestReconnect_GivesUpAtThreshold pre-seeds the failure counter one below
// the limit and points at an unreachable URL: reconnect must return after
// exactly one more attempt with an ErrGiveUpReconnect error (verifiable
// via errors.Is, so callers can branch on fatal-vs-transient).
func TestReconnect_GivesUpAtThreshold(t *testing.T) {
	patchReconnectTunables(t, time.Millisecond, 5*time.Millisecond, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backoff := time.Millisecond
	failures := maxReconnectFailures - 2 // one wait, then give-up
	start := time.Now()
	_, err := reconnect(ctx, ConnectOptions{BackendID: "b-giveup", BackendType: "claude", FrontendURL: "http://127.0.0.1:0"},
		&backoff, &failures, nil)
	if err == nil {
		t.Fatal("expected give-up error, got nil")
	}
	if !errors.Is(err, ErrGiveUpReconnect) {
		t.Fatalf("errors.Is(err, ErrGiveUpReconnect) = false; err = %v", err)
	}
	if failures != maxReconnectFailures {
		t.Fatalf("failures = %d, want %d", failures, maxReconnectFailures)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("give-up took %v, want fast from pre-seeded counter", d)
	}
}

// TestReconnect_FailuresPersistAcrossConnectSuccess pins the symmetric
// counterpart to change A: Connect success must NOT reset the failure
// counter. Only RunWithClient's receive-success path may. Without this, a server
// that handshakes then immediately drops the stream would never reach the
// give-up threshold.
func TestReconnect_FailuresPersistAcrossConnectSuccess(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backoff := 100 * time.Millisecond
	// Pre-seed below the threshold but well above 0; a successful Connect
	// must increment by exactly one (the iter that succeeded), not reset.
	preSeed := 5
	failures := preSeed
	c, err := reconnect(ctx, ConnectOptions{BackendID: "b-persist", BackendType: "claude", FrontendURL: ts.URL},
		&backoff, &failures, nil)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer c.Close()
	if failures != preSeed+1 {
		t.Fatalf("failures = %d after Connect success, want %d (must increment by one, NOT reset to 0)",
			failures, preSeed+1)
	}
}

// TestRun_GivesUpAfterSustainedFailures exercises the end-to-end give-up
// path: with patched tunables, RunWithClient connects to a live frontend, the
// stream is broken, then every subsequent Connect is rejected — after
// maxReconnectFailures attempts RunWithClient returns ErrGiveUpReconnect. The error
// must be wrap-detectable so cmd/* can branch on fatal and let the
// process exit for the supervisor to restart.
//
// A gating handler is used instead of httptest.Server.Close() because
// Close blocks on outstanding SSE requests (which never finish naturally),
// deadlocking the test against RunWithClient's blocked RecvEvent.
func TestRun_GivesUpAfterSustainedFailures(t *testing.T) {
	patchReconnectTunables(t, time.Millisecond, 5*time.Millisecond, 4)

	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	var reject atomic.Bool
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reject.Load() {
			http.Error(w, "frontend down", http.StatusServiceUnavailable)
			return
		}
		srv.Routes().ServeHTTP(w, r)
	})
	ts := httptest.NewServer(wrapped)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	opts := ConnectOptions{BackendID: "b-e2e", BackendType: "claude", FrontendURL: ts.URL}
	client, err := Connect(opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go func() {
		done <- RunWithClient(ctx, client, opts,
			func(context.Context, *protocol.Event) error { return nil }, nil)
	}()

	// Wait for the initial Connect to register (proves the frontend was
	// reachable at startup; the fail-fast path is covered by other tests).
	regReady := time.After(2 * time.Second)
	for {
		if _, ok := reg.Get("b-e2e"); ok {
			break
		}
		select {
		case <-regReady:
			t.Fatal("initial connect never registered")
		case err := <-done:
			t.Fatalf("RunWithClient exited before registration: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Break the live stream AND reject all new requests (including
	// Connect's handshake). RunWithClient enters reconnect, every Connect fails, and
	// after maxReconnectFailures attempts it gives up with ErrGiveUpReconnect.
	reject.Store(true)
	_ = reg.Unregister("b-e2e")

	start := time.Now()
	select {
	case err := <-done:
		if !errors.Is(err, ErrGiveUpReconnect) {
			t.Fatalf("errors.Is(err, ErrGiveUpReconnect) = false; err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunWithClient did not give up after frontend shutdown")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("give-up took %v, want fast with patched tunables", d)
	}
}

// TestRun_RecvSuccessResetsFailures proves the give-up threshold is gated
// on SUSTAINED failure, not lifetime attempts: each delivered event resets
// the counter, so a stream that flaps but recovers between flaps survives
// indefinitely. Cycles > maxReconnectFailures with a tight limit; if the
// reset were broken, RunWithClient would give up mid-loop.
func TestRun_RecvSuccessResetsFailures(t *testing.T) {
	patchReconnectTunables(t, 2*time.Millisecond, 5*time.Millisecond, 3)

	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var got atomic.Int32
	done := make(chan error, 1)
	opts := ConnectOptions{BackendID: "b-rst", BackendType: "claude", FrontendURL: ts.URL}
	client, err := Connect(opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	go func() {
		done <- RunWithClient(ctx, client, opts,
			func(_ context.Context, ev *protocol.Event) error {
				if ev.Type == protocol.TypePing {
					got.Add(1)
				}
				return nil
			}, nil)
	}()

	// Wait for the first registration (Connect returns before the SSE
	// handshake registers, so poll the registry).
	regReady := time.After(2 * time.Second)
	for {
		if _, ok := reg.Get("b-rst"); ok {
			break
		}
		select {
		case <-regReady:
			t.Fatal("backend never registered")
		case err := <-done:
			t.Fatalf("RunWithClient exited before first registration: %v", err)
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// More break/recover cycles than maxReconnectFailures. Each Unregister
	// closes the conn → RecvEvent errors → RunWithClient reconnects → Connect
	// re-registers → we send a Ping → RunWithClient resets failures to 0.
	cycles := maxReconnectFailures + 2
	for range cycles {
		reg.Unregister("b-rst")
		// Wait for RunWithClient's reconnect to re-register.
		rereg := time.After(2 * time.Second)
		for {
			if _, ok := reg.Get("b-rst"); ok {
				break
			}
			select {
			case <-rereg:
				t.Fatal("backend did not re-register after Unregister")
			case err := <-done:
				t.Fatalf("RunWithClient gave up mid-cycle (failures not reset by recv): %v", err)
			default:
				time.Sleep(2 * time.Millisecond)
			}
		}
		if err := reg.SendEvent("b-rst", &protocol.Event{
			Type: protocol.TypePing, Ping: &protocol.PingPayload{},
		}); err != nil {
			t.Fatalf("SendEvent: %v", err)
		}
		// Wait for the ping counter to tick.
		prev := got.Load()
		delivery := time.After(2 * time.Second)
		for got.Load() == prev {
			select {
			case <-delivery:
				t.Fatalf("ping %d not delivered", prev+1)
			case err := <-done:
				t.Fatalf("RunWithClient exited waiting for event (failures not reset): %v", err)
			default:
				time.Sleep(2 * time.Millisecond)
			}
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunWithClient returned error after %d cycles: %v", cycles, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunWithClient did not return after cancel")
	}
}

// TestConnect_PinnedBackendToken verifies ConnectOptions.BackendToken is
// presented verbatim on the SSE handshake (M10-2 stability: the process pins
// one token so reconnects re-register with the same identity the POST paths
// use).
func TestConnect_PinnedBackendToken(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	c, err := Connect(ConnectOptions{
		BackendID: "back-tok", BackendType: "claude", FrontendURL: ts.URL,
		BackendToken: "pinned-token-123",
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	deadline := time.After(2 * time.Second)
	for {
		conn, ok := reg.Get("back-tok")
		if ok {
			if got := conn.Token(); got != "pinned-token-123" {
				t.Fatalf("registered token = %q, want pinned-token-123", got)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("backend never registered")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// TestRunWithClient_ReusesInitialConnection verifies low-9: RunWithClient
// consumes the caller's already-connected client as its first stream instead
// of dialing a second connection (which the frontend would replace-kick and,
// worse, whose token mismatch would get every POST rejected as impersonation).
func TestRunWithClient_ReusesInitialConnection(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := ConnectOptions{
		BackendID: "back-rwc", BackendType: "claude", FrontendURL: ts.URL,
		BackendToken: NewBackendToken(),
	}
	client, err := Connect(opts)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	var got atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- RunWithClient(ctx, client, opts,
			func(_ context.Context, ev *protocol.Event) error {
				if ev.Type == protocol.TypePing {
					got.Add(1)
				}
				return nil
			},
			func(error) {})
	}()

	// The pre-connected client must already be registered — no second
	// handshake needed before events flow.
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := reg.Get("back-rwc"); ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("backend never registered")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := reg.SendEvent("back-rwc", &protocol.Event{Type: protocol.TypePing, Ping: &protocol.PingPayload{}}); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	for got.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for event delivery")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunWithClient returned %v, want nil on cancel", err)
	}
}

// TestRunWithClient_NilClientRejected covers the guard clause.
func TestRunWithClient_NilClientRejected(t *testing.T) {
	err := RunWithClient(context.Background(), nil, ConnectOptions{},
		func(context.Context, *protocol.Event) error { return nil }, nil)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

// TestConnect_TLSClientConfig covers the M10-1 backend-side TLS client:
// garbage CA / cert paths fail at Connect time (not mid-handshake).
func TestConnect_TLSClientConfig(t *testing.T) {
	dir := t.TempDir()
	bad := dir + "/bad.pem"
	if err := os.WriteFile(bad, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(ConnectOptions{
		BackendID: "b", BackendType: "claude", FrontendURL: "https://127.0.0.1:0",
		TLSCAFile: bad,
	}); err == nil {
		t.Fatal("expected error for garbage CA file")
	}
	if _, err := Connect(ConnectOptions{
		BackendID: "b", BackendType: "claude", FrontendURL: "https://127.0.0.1:0",
		TLSClientCertFile: bad, TLSClientKeyFile: bad,
	}); err == nil {
		t.Fatal("expected error for garbage client cert/key")
	}
}
