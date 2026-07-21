package opencodeservebridge

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	oc "github.com/justphantom/opencode-go-sdk-lite"
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

// TestStreamForPoolsPerDirectory: the v1 event bus is isolated by directory,
// so streamFor must hand out one stream per directory — same directory
// (modulo filepath.Clean) reuses the stream, different directories get
// distinct streams, and Close shuts all of them down (idempotently).
func TestStreamForPoolsPerDirectory(t *testing.T) {
	// The streams connect in the background; an unreachable URL is fine —
	// the pool bookkeeping under test never touches the network.
	a, err := NewAgent(AgentConfig{BaseURL: "http://127.0.0.1:1"}, nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	s1 := a.streamFor(&oc.LocationRef{Directory: "/repo/a"})
	s1again := a.streamFor(&oc.LocationRef{Directory: "/repo/a/"})
	if s1 != s1again {
		t.Error("same directory should reuse one stream")
	}
	s2 := a.streamFor(&oc.LocationRef{Directory: "/repo/b"})
	if s1 == s2 {
		t.Error("different directories should get distinct streams")
	}
	sNil := a.streamFor(nil)
	if sNil == s1 || sNil == s2 {
		t.Error("nil location should map to its own server-default stream")
	}
	if len(a.streams) != 3 {
		t.Errorf("streams = %d, want 3", len(a.streams))
	}

	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if len(a.streams) != 0 {
		t.Errorf("streams after Close = %d, want 0", len(a.streams))
	}
	if err := a.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
