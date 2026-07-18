package backendrpc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestClient_EventAndControl wires a real IPCServer to a backendrpc.Client and
// exercises both directions: frontend→backend Event over SSE, backend→frontend
// Control over POST.
func TestClient_EventAndControl(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client, err := Connect("back-1", "claude", ts.URL, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	// Give the SSE read goroutine a moment to attach.
	time.Sleep(50 * time.Millisecond)

	// Frontend sends an Event (user prompt).
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "msg-1", Prompt: &protocol.PromptPayload{ChatID: "c1", Text: "hello"}}
	if err := reg.SendEvent("back-1", ev); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	got, err := client.RecvEvent()
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if got.Type != protocol.TypePrompt || got.Prompt.Text != "hello" {
		t.Fatalf("unexpected event: %+v", got)
	}

	// Client sends a Control (AI text).
	ctrl := &protocol.Control{Type: protocol.TypeText, PromptID: "msg-1", Text: &protocol.TextPayload{Delta: "hi"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.SendControl(ctx, ctrl); err != nil {
		t.Fatalf("SendControl: %v", err)
	}

	select {
	case rc := <-reg.Controls():
		if rc.BackendID != "back-1" || rc.Control.Type != protocol.TypeText || rc.Control.Text.Delta != "hi" {
			t.Fatalf("unexpected routed control: %+v", rc)
		}
		// BackendID must be backfilled by the frontend handler from the URL.
		if rc.Control.BackendID != "back-1" {
			t.Fatalf("BackendID not backfilled: %q", rc.Control.BackendID)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for control")
	}
}

func TestConnect_RequiresAllParams(t *testing.T) {
	cases := []struct {
		id, typ, url string
	}{
		{"", "claude", "http://x"},
		{"b1", "", "http://x"},
		{"b1", "claude", ""},
	}
	for _, c := range cases {
		if _, err := Connect(c.id, c.typ, c.url, ""); err == nil {
			t.Fatalf("expected error for %+v", c)
		}
	}
}

func TestConnect_Non200Handshake(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	// Hit the control endpoint with GET to force a 405, not the SSE 200 path.
	if _, err := Connect("b1", "claude", ts.URL+"/v1/control", ""); err == nil {
		t.Fatal("expected error for non-200 handshake")
	}
}

func TestRecvEvent_AfterClose(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client, err := Connect("b1", "claude", ts.URL, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := client.RecvEvent(); err == nil {
		t.Fatal("expected error after close, got nil")
	}
}

func TestClose_Idempotent(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client, err := Connect("b1", "claude", ts.URL, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close1: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close2: %v", err)
	}
}

func TestSendControl_ValidationFails(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	srv := feishufront.NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client, err := Connect("b1", "claude", ts.URL, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()
	// text control without a text payload fails Validate before the POST.
	if err := client.SendControl(context.Background(), &protocol.Control{Type: protocol.TypeText}); err == nil {
		t.Fatal("expected validation error")
	}
}

// TestReadSSE_MalformedFrameDropped verifies a non-JSON SSE frame is logged and
// dropped (not delivered to RecvEvent, not crashing the read goroutine); a
// subsequent valid frame still comes through.
func TestReadSSE_MalformedFrameDropped(t *testing.T) {
	// Raw SSE server: first an invalid frame, then a valid one, then hold open.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Malformed frame (triggers json.Unmarshal error path).
		fmt.Fprintf(w, "data: {not-json\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Valid frame immediately after.
		fmt.Fprintf(w, "data: {\"type\":\"ping\",\"ping\":{}}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Hold the stream open so readSSE does not EOF before we read.
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := Connect("b1", "claude", srv.URL, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()
	// Connect registers the backend with the real IPCServer; bypass that by
	// reading events directly — the malformed frame must be skipped, the valid
	// ping must arrive.
	got, err := client.RecvEvent()
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if got.Type != protocol.TypePing {
		t.Fatalf("expected the valid ping after the dropped malformed frame, got %+v", got)
	}
}

// TestReadSSE_InvalidEventDropped verifies a JSON-decodable but structurally
// invalid event (unknown type) is rejected by Validate, logged, and skipped;
// a subsequent valid event still comes through. This guards against a future
// nil-guard slip in a handler: invalid events must never reach RecvEvent.
func TestReadSSE_InvalidEventDropped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		// Decodable but unknown event type → Validate rejects it.
		fmt.Fprintf(w, "data: {\"type\":\"bogus\",\"promptID\":\"x\"}\n\n")
		if fl != nil {
			fl.Flush()
		}
		// Valid frame immediately after.
		fmt.Fprintf(w, "data: {\"type\":\"ping\",\"ping\":{}}\n\n")
		if fl != nil {
			fl.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := Connect("b1", "claude", srv.URL, "")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()

	got, err := client.RecvEvent()
	if err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}
	if got.Type != protocol.TypePing {
		t.Fatalf("expected the valid ping after the dropped invalid event, got %+v", got)
	}
}
