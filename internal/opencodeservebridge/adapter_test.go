package opencodeservebridge

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNewSDKClientBasicAuth: when Username/Password are set, the SDK client
// attaches an HTTP Basic Authorization header to every request; when both
// are empty, no Authorization header is sent.
func TestNewSDKClientBasicAuth(t *testing.T) {
	var gotAuth string
	var gotCount int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCount++
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"healthy":true}`))
	}))
	defer srv.Close()

	withAuth, err := newSDKClient(AgentConfig{BaseURL: srv.URL, Username: "opencode", Password: "pw"})
	if err != nil {
		t.Fatalf("newSDKClient with auth: %v", err)
	}
	if err := withAuth.Health(context.Background()); err != nil {
		t.Fatalf("Health with auth: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("opencode:pw"))
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}

	gotAuth = ""
	noAuth, err := newSDKClient(AgentConfig{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("newSDKClient no auth: %v", err)
	}
	if err := noAuth.Health(context.Background()); err != nil {
		t.Fatalf("Health no auth: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
	if gotCount != 2 {
		t.Errorf("requests = %d, want 2", gotCount)
	}
}
