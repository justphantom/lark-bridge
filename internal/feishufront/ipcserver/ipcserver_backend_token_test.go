package ipcserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// TestIPCBackendToken_BindsPOSTToRegistrant verifies M10-2: once a backend
// registers a per-backend session token, a POST must carry it or be rejected
// as an impersonation. Under the shared bearer secret any peer could otherwise
// POST /v1/control/{id} and act as that backend.
func TestIPCBackendToken_BindsPOSTToRegistrant(t *testing.T) {
	const shared = "shared-secret"
	const tok = "backend-1-session-token"

	reg := feishufront.NewBackendRegistry()
	reg.RegisterWithToken("back-1", "claude", tok)
	srv := NewIPCServer(reg, shared)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	post := func(token string) int {
		ctrl := &protocol.Control{Type: protocol.TypeText, BackendID: "back-1",
			Text: &protocol.TextPayload{Delta: "x"}}
		body, _ := json.Marshal(ctrl)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/control/back-1", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+shared)
		if token != "" {
			req.Header.Set(backendTokenHeader, token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if c := post(tok); c != http.StatusAccepted {
		t.Errorf("matching token: status=%d, want 202", c)
	}
	if c := post("not-the-token"); c != http.StatusForbidden {
		t.Errorf("wrong token: status=%d, want 403", c)
	}
	if c := post(""); c != http.StatusForbidden {
		t.Errorf("missing token on a token-registered backend: status=%d, want 403", c)
	}
}

// TestIPCBackendToken_LegacyBackendOptsOut verifies rolling-upgrade
// compatibility: a backend registered WITHOUT a token (old binary, or tests
// using Register) is not opted into the binding, so its POSTs are accepted
// even with no X-Backend-Token header.
func TestIPCBackendToken_LegacyBackendOptsOut(t *testing.T) {
	reg := feishufront.NewBackendRegistry()
	reg.Register("back-1", "claude") // no token → legacy
	srv := NewIPCServer(reg, "shared-secret")
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	ctrl := &protocol.Control{Type: protocol.TypeText, BackendID: "back-1",
		Text: &protocol.TextPayload{Delta: "x"}}
	body, _ := json.Marshal(ctrl)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/control/back-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer shared-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("legacy backend POST: status=%d, want 202", resp.StatusCode)
	}
}

// TestIPCBackendToken_SSERecordsHeader verifies the SSE handshake records the
// per-backend token from the request header, so a subsequent correct-token
// POST is accepted and a wrong-token POST is rejected (end-to-end through the
// HTTP layer, not just RegisterWithToken).
func TestIPCBackendToken_SSERecordsHeader(t *testing.T) {
	const shared = "shared-secret"
	const tok = "via-sse-header"

	reg := feishufront.NewBackendRegistry()
	srv := NewIPCServer(reg, shared)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	// SSE-register back-1 presenting the token via header.
	sseReq, _ := http.NewRequest(http.MethodGet,
		ts.URL+"/v1/events?backendID=back-1&backendType=claude", nil)
	sseReq.Header.Set("Authorization", "Bearer "+shared)
	sseReq.Header.Set(backendTokenHeader, tok)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("sse: %v", err)
	}
	defer sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK {
		t.Fatalf("sse handshake: status=%d, want 200", sseResp.StatusCode)
	}

	conn, ok := reg.Get("back-1")
	if !ok {
		t.Fatal("backend not registered after SSE")
	}
	if conn.Token() != tok {
		t.Errorf("recorded token=%q, want %q", conn.Token(), tok)
	}
}
