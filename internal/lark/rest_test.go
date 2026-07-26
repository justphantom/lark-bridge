package lark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubServer returns a test server whose handler closure can read the request
// and write a canned response. The closure runs under a mutex so concurrent
// requests serialise (tests inspect captured state after the fact).
func stubServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(handler))
}

// tokenServer serves exactly one tenant_access_token response.
func tokenServer(t *testing.T, token string, expire int, calls *int32) *httptest.Server {
	return stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		if r.URL.Path != "/open-apis/auth/v3/tenant_access_token/internal" {
			http.Error(w, "bad path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(tokenResponse{
			Code: 0, Msg: "ok", TenantAccessToken: token, Expire: expire,
		})
	})
}

// TestTokenManager_CachesUntilExpiry verifies a token within its TTL is served
// from cache (one HTTP call) and refreshed only after the refresh-lead window
// crosses expiry.
func TestTokenManager_CachesUntilExpiry(t *testing.T) {
	var calls int32
	srv := tokenServer(t, "t-cache", 7200, &calls)
	defer srv.Close()

	tm := &tokenManager{
		appID: "a", appSecret: "s",
		baseURL: srv.URL,
		http:    srv.Client(),
	}
	ctx := context.Background()
	tok1, err := tm.Token(ctx)
	if err != nil {
		t.Fatalf("Token #1: %v", err)
	}
	if tok1 != "t-cache" {
		t.Fatalf("tok1 = %q", tok1)
	}
	tok2, err := tm.Token(ctx)
	if err != nil {
		t.Fatalf("Token #2: %v", err)
	}
	if tok2 != "t-cache" {
		t.Fatalf("tok2 = %q", tok2)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("token fetch calls = %d, want 1 (cache miss then hit)", got)
	}
}

// TestTokenManager_RefreshesWhenCloseToExpiry pins the lead behaviour: a
// token whose remaining TTL is inside tokenRefreshLead is re-fetched.
func TestTokenManager_RefreshesWhenCloseToExpiry(t *testing.T) {
	mu := sync.Mutex{}
	count := 0
	srv := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		// Short TTL so the refresh-lead window (5min) immediately demands a refresh.
		_ = json.NewEncoder(w).Encode(tokenResponse{
			Code: 0, Msg: "ok", TenantAccessToken: "t-short", Expire: n,
		})
	})
	defer srv.Close()

	tm := &tokenManager{appID: "a", appSecret: "s", baseURL: srv.URL, http: srv.Client()}
	// expire=1s < 5min lead, so every Token call beyond the first triggers a refresh.
	if _, err := tm.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := tm.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if count < 2 {
		t.Fatalf("refresh call count = %d, want >=2 (lead forced refresh)", count)
	}
}

// TestTokenManager_PropagatesBusinessError verifies a non-zero code surfaces as
// *APIError (so the bridge can classify it like a REST error).
func TestTokenManager_PropagatesBusinessError(t *testing.T) {
	srv := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tokenResponse{Code: 99991663, Msg: "invalid app_id"})
	})
	defer srv.Close()
	tm := &tokenManager{appID: "a", appSecret: "s", baseURL: srv.URL, http: srv.Client()}
	_, err := tm.Token(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if ae.Code != 99991663 || !strings.Contains(ae.Msg, "invalid") {
		t.Fatalf("APIError = %+v", ae)
	}
}

// TestEncodeSendContent covers the Text/Card mutual-exclusion and inner-JSON
// wrapping for text payloads.
func TestEncodeSendContent(t *testing.T) {
	cases := []struct {
		name     string
		in       *SendInput
		wantType string
		wantSub  string // substring expected in content
		wantErr  bool
	}{
		{"text", &SendInput{Text: "hi"}, "text", `"text":"hi"`, false},
		{"card", &SendInput{Card: `{"schema":"2.0"}`}, "interactive", `{"schema":"2.0"}`, false},
		{"empty", &SendInput{}, "", "", true},
		{"both", &SendInput{Text: "x", Card: "y"}, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mt, content, err := encodeSendContent(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %s/%s", mt, content)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mt != c.wantType {
				t.Errorf("msg_type = %q, want %q", mt, c.wantType)
			}
			if !strings.Contains(content, c.wantSub) {
				t.Errorf("content = %q, want substring %q", content, c.wantSub)
			}
		})
	}
}

// TestRestClient_SendMessage_Create verifies the create path: the request hits
// /im/v1/messages?receive_id_type=chat_id with receive_id in the body, and the
// returned message_id is propagated.
func TestRestClient_SendMessage_Create(t *testing.T) {
	var seenPath, seenQuery, seenAuth, seenReceiveID, seenMsgType string
	rc := newTestRestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		seenAuth = r.Header.Get("Authorization")
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenReceiveID = body["receive_id"]
		seenMsgType = body["msg_type"]
		_ = json.NewEncoder(w).Encode(imResponse{
			Code: 0, Msg: "ok",
			Data: json.RawMessage(`{"message_id":"om_new"}`),
		})
	})

	res, err := rc.SendMessage(context.Background(), &SendInput{ChatID: "oc_chat", Text: "hello"})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.MessageID != "om_new" {
		t.Fatalf("MessageID = %q", res.MessageID)
	}
	if seenPath != "/open-apis/im/v1/messages" {
		t.Errorf("path = %q", seenPath)
	}
	if seenQuery != "receive_id_type=chat_id" {
		t.Errorf("query = %q", seenQuery)
	}
	if !strings.HasPrefix(seenAuth, "Bearer ") {
		t.Errorf("auth = %q", seenAuth)
	}
	if seenReceiveID != "oc_chat" {
		t.Errorf("receive_id = %q", seenReceiveID)
	}
	if seenMsgType != "text" {
		t.Errorf("msg_type = %q", seenMsgType)
	}
}

// TestRestClient_SendMessage_Reply verifies the reply path uses the dedicated
// /messages/{id}/reply endpoint and omits receive_id.
func TestRestClient_SendMessage_Reply(t *testing.T) {
	var seenPath string
	var sawReceiveID bool
	rc := newTestRestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, sawReceiveID = body["receive_id"]
		_ = json.NewEncoder(w).Encode(imResponse{
			Code: 0,
			Data: json.RawMessage(`{"message_id":"om_reply"}`),
		})
	})

	res, err := rc.SendMessage(context.Background(), &SendInput{
		ChatID: "oc_chat", Text: "hi", ReplyMessageID: "om_root",
	})
	if err != nil {
		t.Fatalf("SendMessage reply: %v", err)
	}
	if res.MessageID != "om_reply" {
		t.Fatalf("MessageID = %q", res.MessageID)
	}
	if seenPath != "/open-apis/im/v1/messages/om_root/reply" {
		t.Errorf("path = %q, want reply endpoint", seenPath)
	}
	if sawReceiveID {
		t.Errorf("reply body should NOT carry receive_id")
	}
}

// TestRestClient_PatchMessage verifies PATCH hits the right path with the
// content body.
func TestRestClient_PatchMessage(t *testing.T) {
	var seenPath, seenMethod, seenContent string
	rc := newTestRestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenMethod = r.Method
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenContent = body["content"]
		_ = json.NewEncoder(w).Encode(imResponse{Code: 0, Msg: "ok"})
	})

	if err := rc.PatchMessage(context.Background(), "om_target", `{"schema":"2.0"}`); err != nil {
		t.Fatalf("PatchMessage: %v", err)
	}
	if seenPath != "/open-apis/im/v1/messages/om_target" {
		t.Errorf("path = %q", seenPath)
	}
	if seenMethod != http.MethodPatch {
		t.Errorf("method = %q", seenMethod)
	}
	if seenContent != `{"schema":"2.0"}` {
		t.Errorf("content = %q", seenContent)
	}
}

// TestRestClient_BusinessErrorIsAPIError verifies a non-zero code is returned
// as *APIError carrying the code, matching the existing substring-based
// classification (e.g. "code:230025" → content-too-large).
func TestRestClient_BusinessErrorIsAPIError(t *testing.T) {
	rc := newTestRestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(imResponse{Code: 230025, Msg: "content too large"})
	})
	err := rc.PatchMessage(context.Background(), "om_x", "{}")
	if err == nil {
		t.Fatal("expected error")
	}
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if ae.Code != 230025 {
		t.Fatalf("code = %d", ae.Code)
	}
	if !strings.Contains(err.Error(), "code:230025") {
		t.Errorf("error string %q missing code:230025 substring", err.Error())
	}
}

// TestRestClient_HTTP5xxWraps verifies a server error is surfaced as a plain
// error (not an *APIError, which is reserved for business codes).
func TestRestClient_HTTP5xxWraps(t *testing.T) {
	rc := newTestRestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway down", http.StatusBadGateway)
	})
	err := rc.PatchMessage(context.Background(), "om_x", "{}")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(*APIError); ok {
		t.Fatalf("5xx must not be *APIError, got %v", err)
	}
}

// newTestRestClient builds a restClient whose tokenManager points at a server
// that serves (a) the token endpoint with a fixed token and (b) the IM
// endpoints via imHandler. Sharing one server mirrors production where auth
// and IM share the same host.
func newTestRestClient(t *testing.T, imHandler func(http.ResponseWriter, *http.Request)) *restClient {
	t.Helper()
	srv := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/open-apis/auth/v3/tenant_access_token/internal":
			_ = json.NewEncoder(w).Encode(tokenResponse{
				Code: 0, Msg: "ok", TenantAccessToken: "t-test", Expire: 7200,
			})
		default:
			imHandler(w, r)
		}
	})
	t.Cleanup(srv.Close)
	tm := &tokenManager{appID: "a", appSecret: "s", baseURL: srv.URL, http: srv.Client()}
	return &restClient{baseURL: srv.URL, http: srv.Client(), tokens: tm}
}

// Ensure io and time are referenced (used by helpers in the package; keeps the
// test file self-contained if the package internals shift).
var _ = io.Discard
var _ = time.Second
