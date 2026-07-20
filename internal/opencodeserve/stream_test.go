package opencodeserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// fakeServe is a minimal mock of the opencode serve server, exposing only
// the endpoints the client touches: GET /event (SSE), POST /session (create),
// POST /session/{id}/prompt_async (true-async, 204), POST /session/{id}/abort,
// and GET /config (health). The SSE script is held until the first prompt
// POST arrives, mirroring the real server's "session.created → prompt →
// stream" ordering so the subscriber is always registered before events flow.
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
		case "prompt_async":
			body, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.msgs = append(f.msgs, sessionMsg{sessionID: sid, body: string(body)})
			// One-shot trigger: close if this is the first prompt POST.
			select {
			case <-f.sseTrigger:
			default:
				close(f.sseTrigger)
			}
			f.mu.Unlock()
			// opencode 1.18 prompt_async returns 204 No Content.
			w.WriteHeader(http.StatusNoContent)
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
		`{"type":"message.part.delta","properties":{"sessionID":"ses_1","messageID":"msg_A","partID":"prt_1","field":"text","delta":"Hello"}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses_1","messageID":"msg_A","partID":"prt_1","field":"text","delta":", world!"}}`,
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
		`{"type":"message.part.updated","properties":{"sessionID":"ses_2","part":{"type":"step-start","messageID":"msg_A"}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_2","part":{"type":"step-finish","messageID":"msg_A","reason":"stop","tokens":{"input":10,"output":5,"cache":{"read":1,"write":2}},"cost":0.001}}}`,
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
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"step-start","messageID":"msg_A"}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"tool","messageID":"msg_A","tool":"bash","state":{"status":"pending","input":{"command":"echo hi"},"raw":""}}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"tool","messageID":"msg_A","tool":"bash","state":{"status":"completed","output":{"stdout":"hi"}}}}}`,
		`{"type":"message.part.updated","properties":{"sessionID":"ses_3","part":{"type":"step-finish","messageID":"msg_A","reason":"stop"}}}`,
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

// lastPromptBody returns the body of the most recent prompt_async POST under
// fakeServe's lock.
func (f *fakeServe) lastPromptBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.msgs) == 0 {
		return ""
	}
	return f.msgs[len(f.msgs)-1].body
}

// TestRun_PromptFirstEvent verifies pump emits EventPrompt as the FIRST
// event on out, carrying the sessionID and a non-empty user messageID. Also
// asserts the prompt_async body carries that messageID, has no "role" field,
// and nests model (when provided) as {providerID, modelID}.
func TestRun_PromptFirstEvent(t *testing.T) {
	script := []string{
		`{"type":"session.created","properties":{"sessionID":"ses_P","info":{"id":"ses_P"}}}`,
		`{"type":"message.part.delta","properties":{"sessionID":"ses_P","messageID":"msg_A","partID":"prt_1","field":"text","delta":"hi"}}`,
		`{"type":"session.idle","properties":{"sessionID":"ses_P"}}`,
	}
	srv := newFakeServe(t, script)
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL()}, nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{SessionID: "ses_P", Prompt: "hi", Model: "zhipuai/glm-5.2"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	first := <-events
	if first.GetType() != EventPrompt {
		t.Fatalf("first event type = %q, want %q", first.GetType(), EventPrompt)
	}
	if first.GetSessionID() != "ses_P" {
		t.Errorf("prompt sessionID = %q, want ses_P", first.GetSessionID())
	}
	promptMsgID := first.GetMessageID()
	if !strings.HasPrefix(promptMsgID, "msg_") || len(promptMsgID) != 30 {
		t.Errorf("prompt messageID = %q, want msg_ + 26 chars", promptMsgID)
	}
	// Drain the rest so pump exits cleanly.
	for ev := range events {
		_ = ev
	}

	// Assert the prompt_async body matches the prompt messageID, has no role,
	// and nests model under {providerID, modelID}.
	body := srv.lastPromptBody()
	if body == "" {
		t.Fatal("no prompt_async body recorded")
	}
	var parsed struct {
		MessageID string `json:"messageID"`
		Role      string `json:"role"`
		Model     *struct {
			ProviderID string `json:"providerID"`
			ModelID    string `json:"modelID"`
		} `json:"model"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("parse prompt body: %v\nbody: %s", err, body)
	}
	if parsed.MessageID != promptMsgID {
		t.Errorf("body messageID = %q, want %q", parsed.MessageID, promptMsgID)
	}
	if parsed.Role != "" {
		t.Errorf("body has role = %q, want absent (additionalProperties:false)", parsed.Role)
	}
	if parsed.Model == nil || parsed.Model.ProviderID != "zhipuai" || parsed.Model.ModelID != "glm-5.2" {
		t.Errorf("body model = %+v, want nested {zhipuai, glm-5.2}", parsed.Model)
	}
}

// TestRun_MessageIDFiltering verifies pump locks the first part event's
// assistant messageID and drops subsequent part events tagged with a
// different messageID (stale or concurrent reply on the same session).
func TestRun_MessageIDFiltering(t *testing.T) {
	script := []string{
		`{"type":"session.created","properties":{"sessionID":"ses_F","info":{"id":"ses_F"}}}`,
		// This round's assistant reply.
		`{"type":"message.part.delta","properties":{"sessionID":"ses_F","messageID":"msg_A","partID":"prt_1","field":"text","delta":"keep-"}}`,
		// Stale event from a different assistant message — must be dropped.
		`{"type":"message.part.delta","properties":{"sessionID":"ses_F","messageID":"msg_OTHER","partID":"prt_2","field":"text","delta":"DROP"}}`,
		// Same-round delta resumes.
		`{"type":"message.part.delta","properties":{"sessionID":"ses_F","messageID":"msg_A","partID":"prt_1","field":"text","delta":"me"}}`,
		`{"type":"session.idle","properties":{"sessionID":"ses_F"}}`,
	}
	srv := newFakeServe(t, script)
	defer srv.Close()

	c, _ := New(Config{BaseURL: srv.URL()}, nil)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{SessionID: "ses_F", Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var text strings.Builder
	for ev := range events {
		if ev.GetType() == EventText {
			text.WriteString(ev.GetText())
		}
	}
	if got := text.String(); got != "keep-me" {
		t.Errorf("text = %q, want %q (stale DROP must be filtered)", got, "keep-me")
	}
}

// TestSSE_HeartbeatForcesReconnect verifies the watchdog cancels the active
// connection when no traffic has arrived within heartbeatTimeout — the only
// way to unblock a half-open TCP read. We shrink heartbeatTimeout to keep
// the test fast and manually backdate lastHeartbeat past the window.
func TestSSE_HeartbeatForcesReconnect(t *testing.T) {
	saved := heartbeatTimeout
	t.Cleanup(func() { heartbeatTimeout = saved })
	heartbeatTimeout = 50 * time.Millisecond

	d := newSSEDispatcher("http://unused", nil, nil)
	// newSSEDispatcher spawned a watchdog; stop it on cleanup so it does
	// not leak. Close stopCh + wait on heartbeatDone directly (we did not
	// start run(), so d.done never closes — do NOT call d.stop() here).
	t.Cleanup(func() {
		close(d.stopCh)
		<-d.heartbeatDone
	})

	cancelled := make(chan struct{})
	d.setConnCancel(func() { close(cancelled) })
	// Backdate heartbeat so the first watchdog tick already sees stale.
	d.lastHeartbeatMu.Lock()
	d.lastHeartbeat = time.Now().Add(-2 * heartbeatTimeout)
	d.lastHeartbeatMu.Unlock()

	select {
	case <-cancelled:
		// watchdog fired cancelConn as expected
	case <-time.After(time.Second):
		t.Fatal("watchdog did not cancel the stale connection within 1s")
	}
}

// TestSSE_RecoverPanic verifies recoverPanic swallows a panic instead of
// propagating — the guarantee that a single malformed frame cannot kill the
// global SSE subscription.
func TestSSE_RecoverPanic(t *testing.T) {
	// A goroutine that panics must not escape.
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverPanic(log.Nop(), "test")
		panic("boom")
	}()
	select {
	case <-done:
		// recoverPanic swallowed the panic as expected.
	case <-time.After(time.Second):
		t.Fatal("recoverPanic let the panic escape (goroutine did not return)")
	}
}

// TestAPIError_SentinelMapping verifies apiError maps HTTP status codes onto
// the sentinel errors so callers can branch with errors.Is instead of string
// matching on the formatted message.
func TestAPIError_SentinelMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"404 -> ErrNotFound", http.StatusNotFound, `{"_tag":"NotFound"}`, ErrNotFound},
		{"401 -> ErrUnauthorized", http.StatusUnauthorized, "token expired", ErrUnauthorized},
		{"409 -> ErrSessionBusy", http.StatusConflict, "session ses_X busy", ErrSessionBusy},
		{"500 -> generic", http.StatusInternalServerError, "boom", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := apiError(tc.status, truncateDetail([]byte(tc.body)))
			if tc.want == nil {
				if errors.Is(err, ErrNotFound) || errors.Is(err, ErrSessionBusy) || errors.Is(err, ErrUnauthorized) {
					t.Fatalf("status %d mapped to a sentinel; want generic: %v", tc.status, err)
				}
				if !strings.Contains(err.Error(), tc.body) {
					t.Errorf("generic error lost body detail: %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is: got %v, want %v", err, tc.want)
			}
			// The wrapped detail must survive so logs stay diagnosable.
			if !strings.Contains(err.Error(), tc.body) {
				t.Errorf("sentinel error lost body detail %q: %v", tc.body, err)
			}
		})
	}
}
