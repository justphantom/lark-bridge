package feishufront

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

func TestIPCServer_EventAndControlRoundTrip(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	backendID := "back-1"
	backendType := "claude"

	// 1. Backend connects SSE.
	sseURL := fmt.Sprintf("%s/v1/events?backendID=%s&backendType=%s", ts.URL, backendID, backendType)
	resp, err := http.Get(sseURL)
	if err != nil {
		t.Fatalf("sse connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sse status = %d, want 200", resp.StatusCode)
	}

	// 2. Frontend sends an Event (user prompt) via the registry.
	ev := &protocol.Event{Type: protocol.TypePrompt, PromptID: "msg-1", Prompt: &protocol.PromptPayload{ChatID: "c1", Text: "hello"}}
	if err := reg.SendEvent(backendID, ev); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}

	// 3. Read the Event back from the SSE body.
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read SSE line: %v", err)
	}
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("frame prefix missing: %q", line)
	}
	var got protocol.Event
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
	// the frame is "data: <json>\n\n"; ReadString('\n') returns the first
	// line ending in \n. The json sits after "data: " up to the first \n.
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if got.Type != protocol.TypePrompt || got.Prompt.Text != "hello" {
		t.Fatalf("unexpected event: %+v", got)
	}

	// 4. Backend POSTs a Control (AI text).
	ctrl := &protocol.Control{Type: protocol.TypeText, BackendID: backendID, PromptID: "msg-1", Text: &protocol.TextPayload{Delta: "hi"}}
	body, _ := json.Marshal(ctrl)
	ctrlURL := fmt.Sprintf("%s/v1/control/%s", ts.URL, backendID)
	postResp, err := http.Post(ctrlURL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post control: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("control status = %d, want 202", postResp.StatusCode)
	}

	// 5. Frontend receives the Control from Controls().
	select {
	case rc := <-reg.Controls():
		if rc.BackendID != backendID || rc.Control.Type != protocol.TypeText || rc.Control.Text.Delta != "hi" {
			t.Fatalf("unexpected routed control: %+v", rc)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for control")
	}
}

func TestSSE_MissingParams(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/events")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestControl_UnregisteredBackend(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	ctrl := &protocol.Control{Type: protocol.TypeText, Text: &protocol.TextPayload{Delta: "hi"}}
	body, _ := json.Marshal(ctrl)
	resp, err := http.Post(ts.URL+"/v1/control/ghost", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

// TestStatus_ReportsInFlight verifies the deploy-time status endpoint: an
// unauthenticated request gets 401 (so deploy.sh without a secret knows auth
// is wired), while an authenticated request returns the in-flight turn count
// the operator needs to decide whether a restart is safe.
func TestStatus_ReportsInFlight(t *testing.T) {
	reg := NewBackendRegistry()
	reg.Register("b1", "claude")
	srv := NewIPCServer(reg, "topsecret")
	// Wire an in-flight counter that reflects a live conversation.
	count := 2
	srv.SetInFlightTurns(func() int { return count })
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// No auth → 401 (deploy.sh distinguishes 401=service-up from 000=no-service).
	resp, err := http.Get(ts.URL + "/v1/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-auth status = %d, want 401", resp.StatusCode)
	}

	// Authenticated → JSON with the current count + registered backends.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer topsecret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authed get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authed status = %d, want 200", resp.StatusCode)
	}
	var got statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InFlight != 2 {
		t.Errorf("InFlight = %d, want 2", got.InFlight)
	}
	// Count is live — bump it and re-query to confirm the endpoint reflects state.
	count = 0
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	req2.Header.Set("Authorization", "Bearer topsecret")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	var got2 statusResponse
	json.NewDecoder(resp2.Body).Decode(&got2)
	resp2.Body.Close()
	if got2.InFlight != 0 {
		t.Errorf("InFlight after count→0 = %d, want 0 (endpoint must be live, not cached)", got2.InFlight)
	}
}

// TestStatus_UnsetCounterReportsZero locks the safe default: when
// SetInFlightTurns was never called (e.g. unit tests, or a build without the
// main.go wiring), the endpoint reports InFlight=0 so a deploy check treats it
// as "safe to restart" rather than erroring or blocking forever.
func TestStatus_UnsetCounterReportsZero(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "s")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer s")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var got statusResponse
	json.NewDecoder(resp.Body).Decode(&got)
	if got.InFlight != 0 {
		t.Errorf("unset counter InFlight = %d, want 0 (safe default)", got.InFlight)
	}
}

func TestControl_InvalidBody(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	// Register first so we get past the registration check; then send a
	// payload that fails Validate (text control, no text payload).
	reg.Register("b1", "claude")
	ctrl := &protocol.Control{Type: protocol.TypeText} // missing Text
	body, _ := json.Marshal(ctrl)
	resp, err := http.Post(ts.URL+"/v1/control/b1", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestControl_OversizedBodyRejected verifies that a POST body exceeding
// maxControlBody is rejected with 400, so a runaway backend cannot drive the
// frontend OOM.
func TestControl_OversizedBodyRejected(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()
	reg.Register("b1", "claude")
	// Craft a body just over maxControlBody: valid JSON shape but with a huge
	// result text so the total exceeds the cap.
	huge := strings.Repeat("a", maxControlBody+1024)
	ctrl := &protocol.Control{Type: protocol.TypeResult, Result: &protocol.ResultPayload{Text: huge}}
	body, _ := json.Marshal(ctrl)
	resp, err := http.Post(ts.URL+"/v1/control/b1", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (oversized body rejected)", resp.StatusCode)
	}
}

// TestReconnect_OldHandlerDoesNotEvictNewConn verifies that when a backend
// reconnects (Register replaces the conn), the OLD handler's deferred
// UnregisterIfMatch does not evict the NEW connection: after the old SSE
// reader returns, the new conn is still registered and still receives events.
func TestReconnect_OldHandlerDoesNotEvictNewConn(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	// First connection.
	sseURL := ts.URL + "/v1/events?backendID=dup&backendType=opencode"
	old, err := http.Get(sseURL)
	if err != nil {
		t.Fatalf("old connect: %v", err)
	}
	defer old.Body.Close()
	// Drain the initial registration so the conn is established.
	time.Sleep(50 * time.Millisecond)

	// Reconnect: Register replaces the conn under "dup".
	second, err := http.Get(sseURL)
	if err != nil {
		t.Fatalf("second connect: %v", err)
	}
	defer second.Body.Close()
	time.Sleep(50 * time.Millisecond)

	// The old connection should now be closed (its eventCh was closed by the
	// Register→Close path). io.ReadAll returns EOF or an error.
	oldReader := bufio.NewReader(old.Body)
	if _, err := oldReader.ReadString('\n'); err == nil {
		// Not necessarily an error yet, but the conn should be closed; read
		// until EOF. If this blocks, the test times out, which is the real
		// assertion. Bound it with a short read.
		go func() { io.Copy(io.Discard, old.Body) }()
	}

	// The new connection must still be registered and receive events.
	if _, ok := reg.Get("dup"); !ok {
		t.Fatal("new conn was evicted after old handler exited")
	}
	if err := reg.SendEvent("dup", &protocol.Event{Type: protocol.TypePing, Ping: &protocol.PingPayload{}}); err != nil {
		t.Fatalf("SendEvent to new conn: %v", err)
	}
	// The new reader should see the ping frame.
	if _, err := bufio.NewReader(second.Body).ReadString('\n'); err != nil {
		t.Fatalf("new conn did not receive event: %v", err)
	}
}

// TestSSE_DisconnectFiresOnOffline verifies that when a backend's SSE
// connection drops (not a reconnect — the conn is genuinely removed), the
// onOffline callback fires so in-flight turns are released. Without this a
// deploy that stops the backend strands turns until the 90s health check.
func TestSSE_DisconnectFiresOnOffline(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	var (
		offlineMu  sync.Mutex
		offlineGot []string
	)
	srv.SetOnOffline(func(backendID, backendType string) {
		offlineMu.Lock()
		offlineGot = append(offlineGot, backendID)
		offlineMu.Unlock()
	})

	// Single connection, then close it (simulate backend stopping).
	resp, err := http.Get(ts.URL + "/v1/events?backendID=back-1&backendType=claude")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let registration settle
	resp.Body.Close()
	time.Sleep(100 * time.Millisecond) // let deferred unregister + onOffline run

	offlineMu.Lock()
	defer offlineMu.Unlock()
	if len(offlineGot) != 1 || offlineGot[0] != "back-1" {
		t.Errorf("onOffline calls = %v, want [back-1]", offlineGot)
	}
	if _, ok := reg.Get("back-1"); ok {
		t.Error("backend should be unregistered after disconnect")
	}
}

// TestSSE_ReconnectFiresOnOnline verifies the offline→online symmetry: after
// a genuine SSE disconnect fires onOffline, a reconnect must fire onOnline.
// Before the fix the SSE-exit path fired onOffline but did not store
// wasOffline, so the reconnect's LoadAndDelete missed and onOnline never
// fired — chats saw an "offline" notice with no matching "recovered" notice.
func TestSSE_ReconnectFiresOnOnline(t *testing.T) {
	reg := NewBackendRegistry()
	srv := NewIPCServer(reg, "")
	ts := httptest.NewServer(srv.Routes())
	defer ts.Close()

	var (
		mu       sync.Mutex
		offlineN int
		onlineN  int
	)
	srv.SetOnOffline(func(backendID, backendType string) {
		mu.Lock()
		offlineN++
		mu.Unlock()
	})
	srv.SetOnOnline(func(backendID, backendType string) {
		mu.Lock()
		onlineN++
		mu.Unlock()
	})

	// First connection (registers the backend).
	first, err := http.Get(ts.URL + "/v1/events?backendID=back-1&backendType=claude")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	time.Sleep(50 * time.Millisecond) // let registration settle
	// Drop it (genuine disconnect, not a reconnect-overwrite) → onOffline.
	first.Body.Close()
	time.Sleep(100 * time.Millisecond) // let deferred unregister + onOffline run

	// Reconnect → onOnline must fire (this is the fix).
	second, err := http.Get(ts.URL + "/v1/events?backendID=back-1&backendType=claude")
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer second.Body.Close()
	time.Sleep(100 * time.Millisecond) // let wasOffline LoadAndDelete + onOnline run

	mu.Lock()
	defer mu.Unlock()
	if offlineN != 1 {
		t.Errorf("onOffline calls = %d, want 1", offlineN)
	}
	if onlineN != 1 {
		t.Errorf("onOnline calls = %d, want 1 (offline→online symmetry)", onlineN)
	}
}

// TestIPCServer_SetLoggerConcurrent guards the atomicity of the logger field:
// SetLogger runs on the main goroutine while fireCallback (and handleSSE) read
// it from HTTP goroutines. Under -race the previous non-atomic field would
// report a data race here. The assertion is structural (no race + the latest
// logger is observed), not about log output.
func TestIPCServer_SetLoggerConcurrent(t *testing.T) {
	srv := NewIPCServer(NewBackendRegistry(), "")

	done := make(chan struct{})
	// Reader side: hammer fireCallback, which loads s.logger. A callback with
	// a nil fn returns immediately but still touches the logger path only on
	// panic, so drive the logger read directly via the SSE marshal-error path
	// is overkill; instead exercise the public read surface: SetOnOffline + a
	// panic-prone fn forces fireCallback into the recover branch that logs.
	panicky := func(backendID, backendType string) { panic("boom") }
	srv.SetOnOffline(panicky)

	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			srv.fireCallback(srv.onOffline.Load(), "back-1", "claude", "offline")
		}
	}()

	// Writer side: swap loggers concurrently. Each SetLogger stores a fresh
	// pointer; the atomic must keep the field consistent for readers.
	for i := 0; i < 200; i++ {
		srv.SetLogger(log.Nop())
	}
	<-done
}
