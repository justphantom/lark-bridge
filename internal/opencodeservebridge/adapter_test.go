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

// providerFixture samples a /provider response: 3 providers, 2 connected
// (connected-a, connected-b), one disconnected. Models include one
// deprecated entry to also exercise the status filter.
var providerFixture = `{
	"all": [
		{"id":"connected-a","name":"A","models":{
			"m-active":{"id":"m-active","providerID":"connected-a","status":"active"},
			"m-deprecated":{"id":"m-deprecated","providerID":"connected-a","status":"deprecated"}
		}},
		{"id":"connected-b","name":"B","models":{
			"m-b1":{"id":"m-b1","providerID":"connected-b","status":"active"}
		}},
		{"id":"disconnected-c","name":"C","models":{
			"m-c1":{"id":"m-c1","providerID":"disconnected-c","status":"active"}
		}}
	],
	"connected":["connected-a","connected-b"]
}`

// TestListModels_FiltersByConnected verifies ListModels drops models whose
// provider is not in the connected set (the 5500+ catalog → picker-card
// overflow root cause). Deprecated models in connected providers are still
// excluded by the status filter.
func TestListModels_FiltersByConnected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider" || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(providerFixture))
	}))
	defer srv.Close()

	a, err := NewAgent(AgentConfig{BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}

	got, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := map[string]bool{
		"connected-a/m-active": false,
		"connected-b/m-b1":     false,
	}
	for _, m := range got {
		if _, ok := want[m]; !ok {
			t.Errorf("unexpected model %q (should be filtered out)", m)
		}
		want[m] = true
	}
	for m, found := range want {
		if !found {
			t.Errorf("missing expected model %q", m)
		}
	}
}

// TestListModels_ConnectedFailureFallsBack verifies that when the connected
// list is empty (e.g. serve cold-start, or response missing the field),
// ListModels falls back to returning all active models rather than an empty
// list — bridgebase.maxQuestionOptions then truncates to keep the card valid.
func TestListModels_ConnectedFailureFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// connected 字段缺失 → SDK 解析为 nil。
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"all":[
			{"id":"p","name":"P","models":{"m1":{"id":"m1","providerID":"p","status":"active"}}}
		]}`))
	}))
	defer srv.Close()

	a, err := NewAgent(AgentConfig{BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	got, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 1 || got[0] != "p/m1" {
		t.Errorf("got=%v, want [p/m1] (fallback when connected is empty)", got)
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
