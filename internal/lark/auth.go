package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// tokenRefreshLead is how long before expiry we proactively refresh the token.
// The Feishu API typically returns a 7200s TTL; a 5-minute lead absorbs clock
// skew and the cost of an in-flight request at the boundary.
const tokenRefreshLead = 5 * time.Minute

// maxAuthBodyBytes caps how much of a token-endpoint response is read. The
// legit body is a tiny JSON blob; the bound defends against a hostile proxy
// or buggy endpoint streaming a multi-GB body to exhaust memory.
const maxAuthBodyBytes = 1 << 20 // 1 MiB

// tokenManager fetches and caches a tenant_access_token for the configured
// app. Only the internal (self-built app) flow is supported, since the bridge
// runs as exactly one self-built app and needs no other token type.
type tokenManager struct {
	appID     string
	appSecret string
	baseURL   string // origin with scheme, e.g. "https://open.feishu.cn"
	http      httpDoer

	mu       sync.Mutex
	cached   string
	expireAt time.Time

	// inflight serialises refresh attempts WITHOUT holding mu across the HTTP
	// call: the first caller that misses the cache does the fetch and closes
	// the shared result channel; concurrent followers wait on it instead of
	// each blocking on mu for the full 30s http.Client.Timeout. The single-
	// flight result is stashed in inflightRes / inflightErr before reset.
	inflight    chan struct{}
	inflightTok string
	inflightErr error
}

// tokenResponse is the body of POST /auth/v3/tenant_access_token/internal.
type tokenResponse struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"` // seconds
}

// Token returns a non-expired tenant_access_token, refreshing on demand.
// The cache check is short-locked; a refresh miss spawns the fetch under a
// single-flight gate (inflight chan) so concurrent callers share one HTTP
// request rather than each waiting mu for the full 30s client timeout.
func (t *tokenManager) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	if t.cached != "" && time.Now().Add(tokenRefreshLead).Before(t.expireAt) {
		tok := t.cached
		t.mu.Unlock()
		return tok, nil
	}
	// A refresh is already in flight: wait on its result, do not start another.
	if t.inflight != nil {
		ch := t.inflight
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.inflightErr != nil {
			return "", t.inflightErr
		}
		return t.inflightTok, nil
	}
	// Leader: open the gate, then release mu for the duration of fetch.
	t.inflight = make(chan struct{})
	t.mu.Unlock()

	resp, err := t.fetch(ctx)

	t.mu.Lock()
	defer t.mu.Unlock()
	t.inflightTok = ""
	t.inflightErr = nil
	if err != nil {
		t.inflightErr = err
		close(t.inflight)
		t.inflight = nil
		return "", err
	}
	t.cached = resp.TenantAccessToken
	t.expireAt = time.Now().Add(time.Duration(resp.Expire) * time.Second)
	t.inflightTok = t.cached
	close(t.inflight)
	t.inflight = nil
	return t.cached, nil
}

// fetch performs a single token request. Bypasses the cache.
func (t *tokenManager) fetch(ctx context.Context) (*tokenResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     t.appID,
		"app_secret": t.appSecret,
	})
	// bytes.NewReader (W7): body is already a []byte; strings.NewReader(string(body)) copied it needlessly.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.baseURL+"/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("lark: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lark: token fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Cap the read: the token endpoint returns a small JSON blob; a hostile
	// or buggy endpoint returning a multi-GB body must not exhaust memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("lark: token read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lark: token http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var out tokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("lark: token decode: %w", err)
	}
	if out.Code != 0 || out.TenantAccessToken == "" {
		return nil, &APIError{Code: out.Code, Msg: out.Msg}
	}
	return &out, nil
}

// APIError is a non-zero Feishu business code returned by any REST endpoint.
// Its Error string embeds the code in the form "code:<N> msg:<M>" so existing
// substring matchers (e.g. isCardContentRejected) keep working without change.
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("code:%d msg:%s", e.Code, e.Msg)
}

// truncate caps s to n bytes for inclusion in error messages so a verbose
// server error page never balloons a returned error.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
