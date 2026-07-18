package feishufront

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// newAuthTestServer builds an IPCServer with the given secret behind a test
// server, plus registers one backend so control POSTs can target it.
func newAuthTestServer(t *testing.T, secret string) (*httptest.Server, *BackendRegistry) {
	t.Helper()
	reg := NewBackendRegistry()
	reg.Register("back-1", "claude")
	srv := NewIPCServer(reg, secret)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts, reg
}

func doSSE(t *testing.T, ts *httptest.Server, authHeader string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("GET", ts.URL+"/v1/events?backendID=back-1&backendType=claude", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("sse get: %v", err)
	}
	return resp
}

func postControl(t *testing.T, ts *httptest.Server, authHeader string) int {
	t.Helper()
	ctrl := &protocol.Control{Type: protocol.TypeText, BackendID: "back-1", Text: &protocol.TextPayload{Delta: "x"}}
	body, _ := json.Marshal(ctrl)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/control/back-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestIPCAuth_SSERejectsMissingToken(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")
	resp := doSSE(t, ts, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("SSE without token: status = %d, want 401", resp.StatusCode)
	}
}

func TestIPCAuth_SSERejectsWrongToken(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")
	resp := doSSE(t, ts, "Bearer wrong")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("SSE wrong token: status = %d, want 401", resp.StatusCode)
	}
}

func TestIPCAuth_SSEAcceptsCorrectToken(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")
	resp := doSSE(t, ts, "Bearer s3cr3t")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE correct token: status = %d, want 200", resp.StatusCode)
	}
}

func TestIPCAuth_ControlRejectsMissingToken(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")
	if got := postControl(t, ts, ""); got != http.StatusUnauthorized {
		t.Fatalf("POST without token: status = %d, want 401", got)
	}
}

func TestIPCAuth_ControlRejectsWrongToken(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")
	if got := postControl(t, ts, "Bearer nope"); got != http.StatusUnauthorized {
		t.Fatalf("POST wrong token: status = %d, want 401", got)
	}
}

func TestIPCAuth_ControlAcceptsCorrectToken(t *testing.T) {
	ts, reg := newAuthTestServer(t, "s3cr3t")
	if got := postControl(t, ts, "Bearer s3cr3t"); got != http.StatusAccepted {
		t.Fatalf("POST correct token: status = %d, want 202", got)
	}
	select {
	case <-reg.Controls():
	case <-time.After(time.Second):
		t.Fatal("control not received")
	}
}

// TestIPCAuth_BearerPrefixRequired ensures a bare token (no "Bearer " prefix)
// is rejected: the header must be well-formed.
func TestIPCAuth_BearerPrefixRequired(t *testing.T) {
	ts, _ := newAuthTestServer(t, "s3cr3t")
	resp := doSSE(t, ts, "s3cr3t")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("SSE bare token (no Bearer): status = %d, want 401", resp.StatusCode)
	}
}

// TestIPCAuth_BackendClientEndToEnd wires a secret-protected IPCServer to a
// backendrpc.Client that carries the matching token, and verifies the
// frontend→backend SSE and backend→frontend POST paths both work under auth.
func TestIPCAuth_BackendClientEndToEnd(t *testing.T) {
	const secret = "shared-secret-xyz"
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, secret)
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	client, err := backendrpc.Connect("back-1", "claude", ts.URL, secret)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer client.Close()
	time.Sleep(50 * time.Millisecond)

	// Frontend→backend over SSE.
	ev := &protocol.Event{Type: protocol.TypePing, Ping: &protocol.PingPayload{}}
	if err := reg.SendEvent("back-1", ev); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	if _, err := client.RecvEvent(); err != nil {
		t.Fatalf("RecvEvent: %v", err)
	}

	// Backend→frontend over POST.
	ctrl := &protocol.Control{Type: protocol.TypeText, PromptID: "p1", Text: &protocol.TextPayload{Delta: "hi"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.SendControl(ctx, ctrl); err != nil {
		t.Fatalf("SendControl: %v", err)
	}
	select {
	case rc := <-reg.Controls():
		if rc.Control.Text.Delta != "hi" {
			t.Fatalf("control delta = %q", rc.Control.Text.Delta)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for control")
	}
}

// TestIPCAuth_WrongSecretHandshakeFails verifies a backend presenting the
// wrong secret is rejected at the SSE handshake (HTTP 401), so Connect fails.
func TestIPCAuth_WrongSecretHandshakeFails(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "correct")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	if _, err := backendrpc.Connect("back-1", "claude", ts.URL, "wrong"); err == nil {
		t.Fatal("expected handshake failure with wrong secret")
	}
}

// TestIPCAuth_NoSecretAllowsAll preserves the loopback-only escape hatch:
// when no secret is configured, requests without a token are accepted.
func TestIPCAuth_NoSecretAllowsAll(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(fmt.Sprintf("%s/v1/events?backendID=b1&backendType=claude", ts.URL))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("no-secret SSE: status = %d, want 200", resp.StatusCode)
	}
}
