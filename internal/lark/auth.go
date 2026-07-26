package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenRefreshLead is how long before expiry we proactively refresh the token.
// The Feishu API typically returns a 7200s TTL; a 5-minute lead absorbs clock
// skew and the cost of an in-flight request at the boundary.
const tokenRefreshLead = 5 * time.Minute

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
}

// tokenResponse is the body of POST /auth/v3/tenant_access_token/internal.
type tokenResponse struct {
	Code              int    `json:"code"`
	Msg               string `json:"msg"`
	TenantAccessToken string `json:"tenant_access_token"`
	Expire            int    `json:"expire"` // seconds
}

// Token returns a non-expired tenant_access_token, refreshing on demand.
// Concurrent callers serialise on mu so only one refresh runs at a time.
func (t *tokenManager) Token(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cached != "" && time.Now().Add(tokenRefreshLead).Before(t.expireAt) {
		return t.cached, nil
	}
	resp, err := t.fetch(ctx)
	if err != nil {
		return "", err
	}
	t.cached = resp.TenantAccessToken
	t.expireAt = time.Now().Add(time.Duration(resp.Expire) * time.Second)
	return t.cached, nil
}

// fetch performs a single token request. Bypasses the cache.
func (t *tokenManager) fetch(ctx context.Context) (*tokenResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"app_id":     t.appID,
		"app_secret": t.appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.baseURL+"/open-apis/auth/v3/tenant_access_token/internal", strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("lark: token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lark: token fetch: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
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
