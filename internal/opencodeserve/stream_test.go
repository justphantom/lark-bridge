package opencodeserve

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeServe is a minimal mock of the opencode serve server, exposing only
// the endpoints the client touches: GET /event (SSE), POST /session (create),
// POST /session/{id}/message?async=true, POST /session/{id}/abort, and
// GET /config (health). The SSE script is held until the first message POST
// arrives, mirroring the real server's "session.created → message → stream"
// ordering so the subscriber is always registered before events flow.
type fakeServe struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	sessions int
	aborts   []string
	msgs     []sessionMsg
	// sseScript is the list of SSE data-line payloads pushed when the next
	// /session/{id}/message arrives. Each message consumes one script.
	sseScript [][]string
	// sseTrigger is closed by the first /session/{id}/message handler to
	// unblock the waiting SSE goroutine.
	sseTrigger chan struct{}
}

type sessionMsg struct {
	sessionID string
	body      string
}

func newFakeServe(t *testing.T, sseScript []string) *fakeServe {
	t.Helper()
	f := &fakeServe{
		t:          t,
		sseScript:  [][]string{sseScript},
		sseTrigger: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"username":"test"}`))
	})
	mux.HandleFunc("/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		f.mu.Lock()
		f.sessions++
		id := fmt.Sprintf("ses_%d", f.sessions)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q}`, id)
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/session/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		sid := parts[0]
		switch parts[1] {
		case "message":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.msgs = append(f.msgs, sessionMsg{sessionID: sid, body: string(body)})
			// One-shot trigger: close if this is the first message POST.
			select {
			case <-f.sseTrigger:
			default:
				close(f.sseTrigger)
			}
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		case "abort":
			f.mu.Lock()
			f.aborts = append(f.aborts, sid)
			f.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("true"))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/event", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("data: {\"type\":\"server.connected\",\"properties\":{}}\n\n"))
		flusher.Flush()
		// Wait for the first message POST so subscribers are registered
		// before the script runs (mirrors the real server's ordering).
		select {
		case <-f.sseTrigger:
		case <-r.Context().Done():
			return
		}
		f.mu.Lock()
		var script []string
		if len(f.sseScript) > 0 {
			script = f.sseScript[0]
			f.sseScript = f.sseScript[1:]
		}
		f.mu.Unlock()
		for _, line := range script {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	f.server = httptest.NewServer(mux)
	return f
}

func (f *fakeServe) URL() string { return f.server.URL }

func (f *fakeServe) Close() { f.server.Close() }

func (f *fakeServe) abortsRecorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.aborts))
	copy(out, f.aborts)
	return out
}

// TestRun_TextOnlyTurn verifies the happy path: a pure chat reply with no
// tool calls terminates correctly when the dispatcher synthesises an
// EventResult from session.idle.
func TestRun_TextOnlyTurn(t *testing.T) {
	script := []string{
		`{"type":"session.created","properties":{"sessionID":"ses_1","info":{"id":"ses_1","model":{"id":"glm-5.2","providerID":"zhipuai"}}}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"text","delta":"Hello"}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses_1","field":"text","delta":", world!"}}`,
		`{"type":"session.idle","properties":{"sessionID":"ses_1"}}`,
	}
	srv := newFakeServe(t, script)
	defer srv.Close()

	c, err := New(Config{BaseURL: srv.URL()}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var (
		gotSession, gotResult bool
		text                  strings.Builder
	)
	for ev := range events {
		switch ev.GetType() {
		case EventSession:
			gotSession = true
			if ev.GetSessionID() != "ses_1" {
				t.Errorf("sessionID = %q, want ses_1", ev.GetSessionID())
			}
		case EventText:
			text.WriteString(ev.GetText())
		case EventResult:
			gotResult = true
		}
	}
	if !gotSession {
		t.Error("did not see session.created")
	}
	if !gotResult {
		t.Error("did not see EventResult (idle should synthesise one)")
	}
	if got := text.String(); got != "Hello, world!" {
		t.Errorf("text = %q, want %q", got, "Hello, world!")
	}
}

// TestRun_StepFinishStopIsResult verifies a step-finish reason=stop yields
// EventResult directly (the dispatcher does NOT need to wait for idle, but
// the idle frame must not produce a duplicate EventResult).
func TestRun_StepFinishStopIsResult(t *testing.T) {
	script := []string{
		`{"type":"session.created","properties":{"sessionID":"ses_2","info":{"id":"ses_2"}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_2","part":{"type":"step-start"}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_2","part":{"type":"step-finish","reason":"stop","tokens":{"input":10,"output":5,"cache":{"read":1,"write":2}},"cost":0.001}}}`,
		`{"type":"session.idle","properties":{"sessionID":"ses_2"}}`,
	}
	srv := newFakeServe(t, script)
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL()}, nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{SessionID: "ses_2", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var resultCount int
	var lastResult Event
	for ev := range events {
		if ev.GetType() == EventResult {
			resultCount++
			lastResult = ev
		}
	}
	if resultCount != 1 {
		t.Errorf("want exactly 1 EventResult, got %d", resultCount)
	}
	if lastResult.GetInputTokens() != 10 {
		t.Errorf("input tokens = %d, want 10", lastResult.GetInputTokens())
	}
}

// TestRun_ToolRoundTrip verifies tool_use → tool_result transitions are
// surfaced correctly (a bash call that completes with stdout).
func TestRun_ToolRoundTrip(t *testing.T) {
	script := []string{
		`{"type":"session.created","properties":{"sessionID":"ses_3","info":{"id":"ses_3"}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"step-start"}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"tool","tool":"bash","state":{"status":"pending","input":{"command":"echo hi"},"raw":""}}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"tool","tool":"bash","state":{"status":"completed","output":{"stdout":"hi"}}}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"step-finish","reason":"stop"}}}`,
		`{"type":"session.idle","properties":{"sessionID":"ses_3"}}`,
	}
	srv := newFakeServe(t, script)
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL()}, nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{SessionID: "ses_3", Prompt: "run it"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var (
		sawUse, sawResult bool
		resultText        string
	)
	for ev := range events {
		switch ev.GetType() {
		case EventToolUse:
			sawUse = true
			if ev.GetToolName() != "bash" {
				t.Errorf("tool name = %q", ev.GetToolName())
			}
		case EventToolResult:
			sawResult = true
			resultText = ev.GetText()
		}
	}
	if !sawUse {
		t.Error("did not see EventToolUse")
	}
	if !sawResult {
		t.Error("did not see EventToolResult")
	}
	if !strings.Contains(resultText, "hi") {
		t.Errorf("tool result text = %q, want 'hi' inside", resultText)
	}
}

// TestRun_AbortCancelsStream verifies that cancelling a Run's ctx POSTs
// /session/{id}/abort and the caller's channel closes.
func TestRun_AbortCancelsStream(t *testing.T) {
	// Script that never emits a terminal event so the only way out is ctx
	// cancellation triggering abort.
	script := []string{
		`{"type":"session.created","properties":{"sessionID":"ses_4","info":{"id":"ses_4"}}}`,
	}
	srv := newFakeServe(t, script)
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL()}, nil)
	defer c.Close()

	ctx, cancel := context.WithCancel(context.Background())
	events, err := c.Run(ctx, RunOptions{SessionID: "ses_4", Prompt: "stall"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Give the dispatcher a moment to deliver session.created, then cancel.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	// Drain; must close within abortTimeout + small grace.
	deadline := time.After(abortTimeout + 2*time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				aborts := srv.abortsRecorded()
				if len(aborts) != 1 || aborts[0] != "ses_4" {
					t.Errorf("aborts = %v, want [ses_4]", aborts)
				}
				return
			}
		case <-deadline:
			t.Fatal("events channel did not close after abort")
		}
	}
}

// TestClient_IsReady verifies the health gate succeeds against a real
// /config responder and fails against a 500.
func TestClient_IsReady(t *testing.T) {
	srv := newFakeServe(t, nil)
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL()}, nil)
	defer c.Close()
	if err := c.IsReady(context.Background()); err != nil {
		t.Errorf("IsReady against healthy server: %v", err)
	}

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer badSrv.Close()
	c2, _ := New(Config{BaseURL: badSrv.URL}, nil)
	defer c2.Close()
	if err := c2.IsReady(context.Background()); err == nil {
		t.Error("IsReady against 500 should fail")
	}
}
