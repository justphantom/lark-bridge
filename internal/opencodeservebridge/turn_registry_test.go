package opencodeservebridge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// TestNewSDKClient_InjectsLogger verifies oc.WithLogger is wired: a logger
// passed to newSDKClient becomes the SDK's internal logger, so its
// connect/dispatch/watchdog debug lines surface in the captured buffer
// instead of the SDK's no-op default. The serve SSE handshake hangs so the
// SDK's connect goroutine emits its "connect attempt" debug line.
func TestNewSDKClient_InjectsLogger(t *testing.T) {
	// slog does not require its handler's writer to be concurrency-safe,
	// but the SDK's connect/run goroutines write concurrently with the
	// test goroutine reading the buffer, so the writer is mutex-guarded.
	w := &lockedWriter{}
	lvl := &slog.LevelVar{}
	lvl.Set(slog.LevelDebug)
	injected := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: lvl}))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client, err := newSDKClient(AgentConfig{BaseURL: srv.URL}, injected)
	if err != nil {
		t.Fatalf("newSDKClient: %v", err)
	}
	s, err := client.NewGlobalEventStream(context.Background(), nil)
	if err != nil {
		t.Fatalf("NewGlobalEventStream: %v", err)
	}
	defer s.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.String(), "connect attempt") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(w.String(), "connect attempt") {
		t.Errorf("expected SDK to log via injected logger; buf=\n%s", w.String())
	}
}

// lockedWriter is a mutex-guarded bytes.Buffer for use as a slog handler
// sink when the SDK's internal goroutines write concurrently with the test
// goroutine reading the output.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestNewSDKClient_NilLoggerKeepsDefault verifies that a nil logger (the
// "no bridge logger wired" case, e.g. raw newSDKClient in a unit test) does
// not panic and still yields a usable client — the SDK falls back to its
// no-op default logger.
func TestNewSDKClient_NilLoggerKeepsDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"healthy":true}`))
	}))
	defer srv.Close()

	client, err := newSDKClient(AgentConfig{BaseURL: srv.URL}, nil)
	if err != nil {
		t.Fatalf("newSDKClient nil logger: %v", err)
	}
	if err := client.Health(context.Background()); err != nil {
		t.Fatalf("Health with nil logger: %v", err)
	}
}

// TestPickLastAssistantText locks in the rescue scan rule: only role ==
// "assistant" messages contribute, the last non-empty FinalText wins (so the
// terminal "stop" step's reply surfaces even when earlier assistant steps
// emitted tool-arg-only messages with empty text), and an empty / user-only
// list yields "" (the rescue path's "do nothing" trigger).
func TestPickLastAssistantText(t *testing.T) {
	tests := []struct {
		name string
		msgs []oc.SessionMessage
		want string
	}{
		{
			name: "nil list",
			want: "",
		},
		{
			name: "user only",
			msgs: []oc.SessionMessage{
				{Info: oc.MessageInfo{Role: "user"}, Parts: parts(`{"type":"text","text":"hi"}`)},
			},
			want: "",
		},
		{
			name: "single assistant",
			msgs: []oc.SessionMessage{
				{Info: oc.MessageInfo{Role: "assistant"}, Parts: parts(`{"type":"text","text":"FINAL"}`)},
			},
			want: "FINAL",
		},
		{
			name: "assistant with empty text skipped",
			msgs: []oc.SessionMessage{
				{Info: oc.MessageInfo{Role: "assistant"}, Parts: parts(`{"type":"tool_use"}`)},
				{Info: oc.MessageInfo{Role: "assistant"}, Parts: parts(`{"type":"text","text":"FINAL"}`)},
			},
			want: "FINAL",
		},
		{
			name: "last non-empty wins",
			msgs: []oc.SessionMessage{
				{Info: oc.MessageInfo{Role: "assistant"}, Parts: parts(`{"type":"text","text":"EARLY"}`)},
				{Info: oc.MessageInfo{Role: "assistant"}, Parts: parts(`{"type":"text","text":"LATE"}`)},
			},
			want: "LATE",
		},
		{
			name: "synthetic text ignored by FinalText",
			msgs: []oc.SessionMessage{
				{Info: oc.MessageInfo{Role: "assistant"}, Parts: parts(`{"type":"text","text":"synth","synthetic":true}`)},
			},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickLastAssistantText(tc.msgs); got != tc.want {
				t.Errorf("pickLastAssistantText = %q, want %q", got, tc.want)
			}
		})
	}
}

// parts builds []json.RawMessage from raw JSON object fragments, so a
// SessionMessage fixture reads like its wire form rather than a Go struct
// literal. Each entry must be a valid JSON object.
func parts(parts ...string) []json.RawMessage {
	out := make([]json.RawMessage, len(parts))
	for i, p := range parts {
		out[i] = json.RawMessage(p)
	}
	return out
}

// TestAgent_TurnRegistry_Lifecycle pins RegisterTurn / LookupTurn /
// UnregisterTurn: empty sessionID is rejected, a registered turn is
// lookup-able, UnregisterTurn clears it, and re-registration preserves the
// rescued flag (so a stream that re-runs through the same sessionID after
// rescue does not un-mark it).
func TestAgent_TurnRegistry_Lifecycle(t *testing.T) {
	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: "http://127.0.0.1:1"}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()

	// Empty sessionID is a silent no-op (defensive against streamRun seeing
	// a session-less first event).
	a.RegisterTurn("", &turnContext{chatID: "x"})
	if got := a.LookupTurn(""); got != nil {
		t.Errorf("LookupTurn('') = %v, want nil", got)
	}

	a.RegisterTurn("s1", &turnContext{chatID: "c1", replyToID: "r1", modelSpec: "m1", sessionID: "s1"})
	got := a.LookupTurn("s1")
	if got == nil || got.chatID != "c1" {
		t.Fatalf("LookupTurn(s1) = %+v, want chatID=c1", got)
	}

	// Re-register preserves rescued: simulate an already-rescued turn and
	// overwrite the entry; the new entry must keep rescued=true so a
	// subsequent emitTerminal pass still treats it as rescued.
	got.rescued.Store(true)
	a.RegisterTurn("s1", &turnContext{chatID: "c1-new"})
	if renewed := a.LookupTurn("s1"); renewed == nil || !renewed.rescued.Load() {
		t.Errorf("re-register lost rescued flag: %+v", renewed)
	}

	a.UnregisterTurn("s1")
	if got := a.LookupTurn("s1"); got != nil {
		t.Errorf("LookupTurn after Unregister = %v, want nil", got)
	}
	// Unregister of an absent key is a no-op.
	a.UnregisterTurn("never")
}

// rescueRecorder is a rescueSink fake that captures every call. The slice
// grows under a mutex because the SDK's watchdog goroutine and the test's
// main goroutine both touch it.
type rescueRecorder struct {
	mu    sync.Mutex
	calls []rescuedCall
}

type rescuedCall struct {
	turn      *turnContext
	reply     string
	modelSpec string
}

func (r *rescueRecorder) sink() rescueSink {
	return func(_ context.Context, turn *turnContext, reply, modelSpec string) {
		r.mu.Lock()
		r.calls = append(r.calls, rescuedCall{turn: turn, reply: reply, modelSpec: modelSpec})
		r.mu.Unlock()
	}
}

func (r *rescueRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// messagesServer returns an httptest server that responds to
// GET /session/{id}/message with the given JSON body, recording the last
// limit= query string it saw. Used by the handleOnIdle tests.
func messagesServer(t *testing.T, body string) (*httptest.Server, *atomic.Value, *atomic.Int32) {
	t.Helper()
	var lastLimit atomic.Value
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		lastLimit.Store(r.URL.Query().Get("limit"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &lastLimit, &hits
}

// TestAgent_HandleOnIdle_HitFinalText verifies the rescue happy path: when
// the serve history contains an assistant message with non-empty FinalText,
// handleOnIdle marks the turn rescued and forwards the reply via the sink
// with modelSpec passed through. The Limit query is set to rescueListLimit.
func TestAgent_HandleOnIdle_HitFinalText(t *testing.T) {
	body := `[
		{"info":{"id":"m1","role":"user"},"parts":[{"type":"text","text":"hi"}]},
		{"info":{"id":"m2","role":"assistant"},"parts":[{"type":"text","text":"FINAL REPLY"}]}
	]`
	srv, lastLimit, hits := messagesServer(t, body)

	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: srv.URL}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()
	rec := &rescueRecorder{}
	a.SetRescueSink(rec.sink())

	a.RegisterTurn("sess-hit", &turnContext{
		chatID: "chat-hit", replyToID: "msg-hit",
		modelSpec: "p/m", sessionID: "sess-hit",
	})
	a.handleOnIdle("sess-hit", time.Now())

	if rec.count() != 1 {
		t.Fatalf("sink calls = %d, want 1", rec.count())
	}
	call := rec.calls[0]
	if call.reply != "FINAL REPLY" {
		t.Errorf("reply = %q, want 'FINAL REPLY'", call.reply)
	}
	if call.modelSpec != "p/m" {
		t.Errorf("modelSpec = %q, want 'p/m'", call.modelSpec)
	}
	if call.turn.chatID != "chat-hit" {
		t.Errorf("turn.chatID = %q, want chat-hit", call.turn.chatID)
	}
	if got := lastLimit.Load(); got != "20" {
		t.Errorf("ListMessages limit = %v, want '20'", got)
	}
	// rescued flag set: a second tick must skip.
	a.handleOnIdle("sess-hit", time.Now())
	if rec.count() != 1 {
		t.Errorf("second handleOnIdle should skip; calls = %d, want 1", rec.count())
	}
	if hits.Load() != 1 {
		t.Errorf("serve hits = %d, want 1 (second tick must not re-fetch)", hits.Load())
	}
}

// TestAgent_HandleOnIdle_NoAssistantText verifies the "do nothing" branch:
// when ListMessages returns no recoverable assistant text (empty list, user-
// only, or assistant with empty FinalText), the sink is NOT called and the
// rescued flag stays false so a later tick (after the assistant text lands)
// can still rescue.
func TestAgent_HandleOnIdle_NoAssistantText(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty list", `[]`},
		{"user only", `[{"info":{"id":"m1","role":"user"},"parts":[{"type":"text","text":"hi"}]}]`},
		{"assistant empty text", `[{"info":{"id":"m1","role":"assistant"},"parts":[{"type":"tool_use"}]}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := messagesServer(t, tc.body)
			a, err := NewAgent(context.Background(), AgentConfig{BaseURL: srv.URL}, log.Nop())
			if err != nil {
				t.Fatalf("NewAgent: %v", err)
			}
			defer a.Close()
			rec := &rescueRecorder{}
			a.SetRescueSink(rec.sink())

			a.RegisterTurn("sess-empty", &turnContext{
				chatID: "c", replyToID: "r", sessionID: "sess-empty",
			})
			a.handleOnIdle("sess-empty", time.Now())

			if rec.count() != 0 {
				t.Errorf("sink called %d times, want 0", rec.count())
			}
			if turn := a.LookupTurn("sess-empty"); turn != nil && turn.rescued.Load() {
				t.Error("rescued flag set despite no assistant text")
			}
		})
	}
}

// TestAgent_HandleOnIdle_UnregisteredSession verifies a sessionID with no
// registered turn is a no-op: the watchdog can fire for any sessionID the
// stream is subscribed to (e.g. a turn that already returned and cleared
// its slot), and rescue must not fetch the history or call the sink.
func TestAgent_HandleOnIdle_UnregisteredSession(t *testing.T) {
	srv, _, hits := messagesServer(t, `[{"info":{"id":"m1","role":"assistant"},"parts":[{"type":"text","text":"X"}]}]`)
	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: srv.URL}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()
	rec := &rescueRecorder{}
	a.SetRescueSink(rec.sink())

	a.handleOnIdle("never-registered", time.Now())
	if rec.count() != 0 {
		t.Errorf("sink called for unregistered session: %d", rec.count())
	}
	if hits.Load() != 0 {
		t.Errorf("serve hit for unregistered session: %d", hits.Load())
	}
}

// TestAgent_HandleOnIdle_EmptySessionID pins the empty-sessionID guard:
// the watchdog can fire with an empty sessionID for a server-wide idle, and
// rescue must not blow up on the empty map key.
func TestAgent_HandleOnIdle_EmptySessionID(t *testing.T) {
	srv, _, hits := messagesServer(t, `[]`)
	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: srv.URL}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()

	a.handleOnIdle("", time.Now()) // must not panic
	if hits.Load() != 0 {
		t.Errorf("serve hit on empty sessionID: %d", hits.Load())
	}
}

// TestAgent_HandleOnIdle_ListMessagesError verifies that a serve error on
// the history fetch (network / 5xx) is swallowed without calling the sink
// or marking rescued: the next watchdog tick retries.
func TestAgent_HandleOnIdle_ListMessagesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: srv.URL}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()
	rec := &rescueRecorder{}
	a.SetRescueSink(rec.sink())

	a.RegisterTurn("sess-err", &turnContext{chatID: "c", replyToID: "r", sessionID: "sess-err"})
	a.handleOnIdle("sess-err", time.Now())

	if rec.count() != 0 {
		t.Errorf("sink called on ListMessages error: %d", rec.count())
	}
	if turn := a.LookupTurn("sess-err"); turn != nil && turn.rescued.Load() {
		t.Error("rescued flag set on ListMessages error")
	}
}

// TestAgent_HandleOnIdle_NoSink verifies that a nil sink (Handler not wired
// — e.g. a unit test driving Agent directly) is tolerated: rescued is still
// marked so emitTerminal's dedup path works, but no panic on nil call.
func TestAgent_HandleOnIdle_NoSink(t *testing.T) {
	body := `[{"info":{"id":"m1","role":"assistant"},"parts":[{"type":"text","text":"FINAL"}]}]`
	srv, _, _ := messagesServer(t, body)
	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: srv.URL}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()
	// No SetRescueSink call: a.rescue stays nil.

	a.RegisterTurn("sess-nosink", &turnContext{chatID: "c", sessionID: "sess-nosink"})
	a.handleOnIdle("sess-nosink", time.Now()) // must not panic

	turn := a.LookupTurn("sess-nosink")
	if turn == nil || !turn.rescued.Load() {
		t.Error("rescued should be set even when sink is nil (so emitTerminal still dedups)")
	}
}

// TestAgent_HandleOnIdle_RaceTurnUnregisteredDuringListMessages pins the
// double-emit fix: handleOnIdle snapshots the turn pointer before calling
// ListMessages; if the streamRun path returns concurrently (SSE reconnect
// drains a terminal event, emitTerminal's default branch fires), it calls
// UnregisterTurn and the snapshot goes stale. The post-ListMessages guard
// (re-check map identity + CAS on rescued) must keep handleOnIdle from
// flipping rescued and calling the sink — otherwise the user sees A (from
// emitTerminal's default) + B (from the rescue sink) for one turn.
//
// Sequencing is forced with a blocking httptest handler: ListMessages enters
// and parks on releaseCh; the test fires UnregisterTurn, then releases the
// handler so it returns a non-empty assistant reply. The assertions hold
// regardless of OS scheduling because the Unregister-before-release ordering
// is enforced by the test's own statements, not by sleeps.
func TestAgent_HandleOnIdle_RaceTurnUnregisteredDuringListMessages(t *testing.T) {
	releaseCh := make(chan struct{})
	inFlight := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case inFlight <- struct{}{}:
		default:
		}
		<-releaseCh
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"info":{"id":"m1","role":"assistant"},"parts":[{"type":"text","text":"REPLY"}]}]`))
	}))
	defer srv.Close()

	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: srv.URL}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()
	rec := &rescueRecorder{}
	a.SetRescueSink(rec.sink())

	a.RegisterTurn("sess-race", &turnContext{
		chatID: "c", replyToID: "r", sessionID: "sess-race",
	})
	// Snapshot the pointer so we can assert rescued stayed false on the
	// stale pointer after UnregisterTurn dropped it from the map.
	turnBefore := a.LookupTurn("sess-race")
	if turnBefore == nil {
		t.Fatal("LookupTurn before race returned nil")
	}

	done := make(chan struct{})
	go func() {
		a.handleOnIdle("sess-race", time.Now())
		close(done)
	}()

	// Wait until ListMessages has actually entered the handler so the
	// subsequent UnregisterTurn is guaranteed to race against the in-flight
	// request rather than racing handleOnIdle's pre-ListMessages lookup.
	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("ListMessages never reached the test server")
	}

	// Simulate streamRun returning and the runPrompt cleanup dropping the
	// turn entry while handleOnIdle is still parked on ListMessages.
	a.UnregisterTurn("sess-race")

	// Release ListMessages to return its (non-empty) assistant reply.
	close(releaseCh)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleOnIdle did not return after release")
	}

	if rec.count() != 0 {
		t.Errorf("sink called after UnregisterTurn: %d calls (double-emit regression)", rec.count())
	}
	if turnBefore.rescued.Load() {
		t.Error("stale turn pointer's rescued flag was set despite UnregisterTurn racing the sink")
	}
	if got := a.LookupTurn("sess-race"); got != nil {
		t.Errorf("LookupTurn after Unregister = %v, want nil", got)
	}
}

// TestHandler_HandleRescue_EmitsTypeResult verifies the Handler's rescue
// sink assembles a TypeResult bound to the turn's chatID/replyToID and
// surfaces the recovered reply (with StripThinking applied). The wired
// controlCapture mirrors the production backendrpc emit path.
func TestHandler_HandleRescue_EmitsTypeResult(t *testing.T) {
	h, _, captured := newWireHandler(t, closedStreamOpencode{})

	turn := &turnContext{
		chatID:    "chat-rescue",
		replyToID: "msg-rescue",
		modelSpec: "p/m",
		sessionID: "sess-rescue",
	}
	h.handleRescue(context.Background(), turn, "final answer", "p/m")

	ctrl := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeResult && c.ChatID == "chat-rescue"
	}, 2*time.Second)
	if ctrl == nil {
		t.Fatalf("TypeResult not emitted; captured=%+v", captured.find(func(*protocol.Control) bool { return true }))
	}
	if ctrl.PromptID != "msg-rescue" {
		t.Errorf("PromptID = %q, want msg-rescue", ctrl.PromptID)
	}
	if ctrl.Result == nil {
		t.Fatal("Result payload nil")
	}
	if ctrl.Result.Text != "final answer" {
		t.Errorf("Text = %q, want 'final answer'", ctrl.Result.Text)
	}
	if ctrl.Result.Model != "p/m" {
		t.Errorf("Model = %q, want 'p/m'", ctrl.Result.Model)
	}
	if ctrl.Result.SessionID != "sess-rescue" {
		t.Errorf("SessionID = %q, want sess-rescue", ctrl.Result.SessionID)
	}
}

// TestHandler_NewWithLogger_AutoWiresRescue verifies the assembly path:
// when the api implements rescuable (production *Agent does), NewWithLogger
// auto-injects the Handler's rescue sink so the OnIdle watchdog reaches the
// emit path without any explicit wiring in main.go.
func TestHandler_NewWithLogger_AutoWiresRescue(t *testing.T) {
	a, err := NewAgent(context.Background(), AgentConfig{BaseURL: "http://127.0.0.1:1"}, log.Nop())
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer a.Close()

	r, err := newTestRouter()
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := NewWithLogger(r, a, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	defer h.Close()

	a.rescueMu.Lock()
	wired := a.rescue
	a.rescueMu.Unlock()
	if wired == nil {
		t.Error("NewWithLogger did not wire rescue sink for a real *Agent")
	}
}

// TestHandler_NewWithLogger_FakeSkipsRescue verifies the converse: a fake
// opencodeAPI that does not implement rescuable (closedStreamOpencode here)
// does not break NewWithLogger — the rescue path stays nil and the rest of
// the Handler works as before.
func TestHandler_NewWithLogger_FakeSkipsRescue(t *testing.T) {
	r, err := newTestRouter()
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	defer h.Close()
	// No assertion needed beyond "did not panic"; reaching here is success.
}

// newTestRouter is a tiny helper for tests that need a real *router.Router
// but do not care about its contents.
func newTestRouter() (*router.Router, error) {
	return router.New("", log.Nop())
}
