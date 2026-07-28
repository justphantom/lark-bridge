package feishufront

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newPreflightServer builds an IPCServer with a shared secret, a
// deploy-monitor backend registered (so BackendType resolves), and a canned
// in-flight turn list.
func newPreflightServer(t *testing.T, turns []Turn) *httptest.Server {
	t.Helper()
	reg := NewBackendRegistry()
	reg.Register("deploy-monitor-1", "deploy-monitor")
	srv := NewIPCServer(reg, "s3cret")
	srv.SetInFlightDetail(func() []Turn { return turns })
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	return ts
}

func preflightGet(t *testing.T, ts *httptest.Server, services, auth string) (int, preflightResponse) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/v1/deploy-preflight?services=%s", ts.URL, services), nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var out preflightResponse
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusConflict {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return resp.StatusCode, out
}

func TestDeployPreflight(t *testing.T) {
	turns := []Turn{
		{PromptID: "p1", ChatID: "c1", BackendID: "claude-1", StartedAt: time.Now()},
		{PromptID: "p2", ChatID: "c2", BackendID: "opencode-1", StartedAt: time.Now()},
		{PromptID: "p3", ChatID: "c3", BackendID: "deploy-monitor-1", StartedAt: time.Now()}, // excluded
	}
	ts := newPreflightServer(t, turns)

	cases := []struct {
		name         string
		services     string
		wantCode     int
		wantInflight int
		wantAffected []string
	}{
		{"non-target backend only", "miniagent", http.StatusOK, 2, []string{}},
		{"target backend conflict", "claude", http.StatusConflict, 2, []string{"claude-1"}},
		{"feishu disrupts all", "feishu", http.StatusConflict, 2, []string{"claude-1", "opencode-1"}},
		{"subset mixed", "claude,miniagent", http.StatusConflict, 2, []string{"claude-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out := preflightGet(t, ts, tc.services, "Bearer s3cret")
			if code != tc.wantCode {
				t.Errorf("code = %d, want %d", code, tc.wantCode)
			}
			if out.InFlight != tc.wantInflight {
				t.Errorf("inflight = %d, want %d (deploy-monitor excluded)", out.InFlight, tc.wantInflight)
			}
			if fmt.Sprint(out.Affected) != fmt.Sprint(tc.wantAffected) {
				t.Errorf("affected = %v, want %v", out.Affected, tc.wantAffected)
			}
			if tc.wantCode == http.StatusConflict && out.Reason == "" {
				t.Error("409 without a pre-rendered reason")
			}
		})
	}
}

func TestDeployPreflight_IdleAndGuards(t *testing.T) {
	ts := newPreflightServer(t, nil)

	code, out := preflightGet(t, ts, "feishu", "Bearer s3cret")
	if code != http.StatusOK || out.InFlight != 0 || len(out.Affected) != 0 {
		t.Errorf("idle: code=%d out=%+v, want 200/empty", code, out)
	}

	if code, _ := preflightGet(t, ts, "feishu", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("bad secret: code = %d, want 401", code)
	}
	if code, _ := preflightGet(t, ts, "", "Bearer s3cret"); code != http.StatusBadRequest {
		t.Errorf("empty services: code = %d, want 400", code)
	}
}

func TestStripInstanceSuffix(t *testing.T) {
	cases := map[string]string{
		"claude-1":         "claude",
		"opencode-12":      "opencode",
		"deploy-monitor-1": "deploy-monitor",
		"status-monitor":   "status-monitor",
		"claude":           "claude",
		"claude-":          "claude-",
		"-1":               "-1",
	}
	for in, want := range cases {
		if got := stripInstanceSuffix(in); got != want {
			t.Errorf("stripInstanceSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
