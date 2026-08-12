package backendrpc

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestReadSSE_StalledConsumerForcesReconnect pins the C1 fix.
//
// Before the fix, readSSE blocked forever pushing onto a full eventCh when
// the handler loop stopped draining (e.g. a hung CLI child): the select had
// only the eventCh and closeCh branches, so a stalled consumer parked the
// readSSE goroutine and its held resp.Body forever — a goroutine + TCP
// connection leak per stuck turn. The sseSendTimeout branch closes the
// client so a fresh stream can recover.
//
// This test feeds >buffer-capacity events while never draining eventCh,
// shrinks sseSendTimeout to a test value, and asserts readSSE calls Close
// (closeCh closes) promptly instead of parking forever.
func TestReadSSE_StalledConsumerForcesReconnect(t *testing.T) {
	// Shrink the timeout so the test runs in milliseconds, not 30s. Restore
	// the production value on exit so other tests are unaffected.
	prev := sseSendTimeout
	sseSendTimeout = 50 * time.Millisecond
	t.Cleanup(func() { sseSendTimeout = prev })

	// A bare client (no Connect / no IPC registration). readSSE runs on a
	// pipe we control so the test is deterministic.
	c := &Client{
		backendID: "b1",
		eventCh:   make(chan *protocol.Event, sseEventChanBuf),
		closeCh:   make(chan struct{}),
	}
	c.logger.Store(log.Nop())

	// SSE body: more events than the 256-slot buffer. The first 256 fill
	// eventCh (no consumer is draining), so the 257th send must park until
	// sseSendTimeout fires → Close → readSSE returns. We keep the body open
	// (via a blocked goroutine) so the stall is only resolved by the
	// timeout, not by EOF.
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		for range sseEventChanBuf + 10 {
			fmt.Fprintf(pw, "data: {\"type\":\"ping\",\"ping\":{}}\n\n")
		}
		// Hold the read side open so readSSE cannot EOF before the stall.
		select {}
	}()

	done := make(chan struct{})
	go func() { c.readSSE(pr); close(done) }()

	select {
	case <-done:
		// readSSE returned — the stall path fired and called Close. Verify
		// closeCh is closed (Close's observable side effect).
		select {
		case <-c.closeCh:
		default:
			t.Fatal("readSSE returned but closeCh not closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("readSSE did not force a reconnect within timeout (C1 stall regression)")
	}
}

// TestReadSSE_NormalSendNoTimeout is the complement: under normal draining,
// the sseSendTimeout branch never fires and events flow through RecvEvent.
// Guards against an accidental inversion that always closes.
func TestReadSSE_NormalSendNoTimeout(t *testing.T) {
	prev := sseSendTimeout
	sseSendTimeout = 100 * time.Millisecond
	t.Cleanup(func() { sseSendTimeout = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: {\"type\":\"ping\",\"ping\":{}}\n\n")
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := Connect(ConnectOptions{BackendID: "b1", BackendType: "claude", FrontendURL: srv.URL})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	// Drain the event promptly; the send must succeed well within the 100ms
	// timeout, so the stall branch never fires. RecvEvent is a plain call
	// (not a channel op), so run it in a goroutine and race it against the
	// premature-close and timeout cases.
	type recv struct {
		ev  *protocol.Event
		err error
	}
	res := make(chan recv, 1)
	go func() {
		ev, err := client.RecvEvent()
		res <- recv{ev, err}
	}()
	select {
	case <-client.closeCh:
		t.Fatal("client closed prematurely; normal send should not time out")
	case r := <-res:
		if r.err != nil {
			t.Fatalf("RecvEvent: %v", r.err)
		}
		if r.ev == nil || r.ev.Type != "ping" {
			t.Fatalf("expected ping, got %+v", r.ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event never delivered")
	}
}
