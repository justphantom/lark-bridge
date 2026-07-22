package opencodeservebridge

import (
	"context"
	"errors"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// fakeListAgent implements opencodeAPI for testing cmdListSessions. It
// records the directory passed to ListSessions so tests can assert the
// command forwards the chat's working directory.
type fakeListAgent struct {
	sessions []oc.SessionInfo
	err      error
	lastDir  string
	calls    int
}

func (f *fakeListAgent) Run(ctx context.Context, opts oc.RunOptions) (<-chan oc.HighEvent, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeListAgent) ListModels(ctx context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeListAgent) ListAgents(ctx context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeListAgent) AbortSession(ctx context.Context, sessionID string) error {
	return errors.New("not implemented")
}

func (f *fakeListAgent) ListSessions(ctx context.Context, directory string) ([]oc.SessionInfo, error) {
	f.calls++
	f.lastDir = directory
	if f.err != nil {
		return nil, f.err
	}
	return f.sessions, nil
}

func (f *fakeListAgent) SessionStatuses(ctx context.Context) (map[string]oc.SessionStatus, error) {
	return nil, errors.New("not implemented")
}

func (f *fakeListAgent) DeleteSessionIfIdle(ctx context.Context, sessionID string) error {
	return errors.New("not implemented")
}

func (f *fakeListAgent) ReplyPermission(ctx context.Context, requestID, reply, message string) error {
	return errors.New("not implemented")
}

func (f *fakeListAgent) ReplyQuestion(ctx context.Context, requestID string, r *oc.QuestionReply) error {
	return errors.New("not implemented")
}

func (f *fakeListAgent) RejectQuestion(ctx context.Context, requestID string) error {
	return errors.New("not implemented")
}

// TestCmdListSessions_NoBinding verifies a chat with no binding gets the /cd
// hint and the command stays read-only (no ListSessions call, no lazy bind).
func TestCmdListSessions_NoBinding(t *testing.T) {
	agent := &fakeListAgent{}
	h, r := newHandlerWithAgent(t, agent)

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !contains(res.Body, "/cd") {
		t.Errorf("Body = %q, want /cd hint", res.Body)
	}
	if agent.calls != 0 {
		t.Errorf("ListSessions called %d times, want 0 without a binding", agent.calls)
	}
	if _, ok := r.Lookup("chat-1"); ok {
		t.Error("read-only /session-list must not create a binding")
	}
}

// TestCmdListSessions_BindingWithoutDirectory verifies a binding lacking a
// working directory also gets the /cd hint instead of a serve call.
func TestCmdListSessions_BindingWithoutDirectory(t *testing.T) {
	agent := &fakeListAgent{}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "sess-1", "", "", "", "")

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !contains(res.Body, "/cd") {
		t.Errorf("Body = %q, want /cd hint", res.Body)
	}
	if agent.calls != 0 {
		t.Errorf("ListSessions called %d times, want 0 without a directory", agent.calls)
	}
}

// TestCmdListSessions_Empty verifies the command reports the directory-scoped
// empty state.
func TestCmdListSessions_Empty(t *testing.T) {
	agent := &fakeListAgent{sessions: []oc.SessionInfo{}}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "", "/proj", "", "", "")

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if res.Body != "当前目录下没有任何会话。" {
		t.Errorf("Body = %q, want '当前目录下没有任何会话。'", res.Body)
	}
	if agent.lastDir != "/proj" {
		t.Errorf("ListSessions directory = %q, want /proj", agent.lastDir)
	}
}

// TestCmdListSessions_Error verifies the command reports list errors.
func TestCmdListSessions_Error(t *testing.T) {
	agent := &fakeListAgent{err: errors.New("连接失败")}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "", "/proj", "", "", "")

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "连接失败" {
		t.Errorf("err = %v, want '连接失败'", err)
	}
	if res.Body == "" {
		t.Error("expected non-empty body even on error")
	}
}

// TestCmdListSessions_ListsSessions verifies sessions are listed with all fields.
func TestCmdListSessions_ListsSessions(t *testing.T) {
	now := time.Now().UnixMilli()
	sessions := []oc.SessionInfo{
		{
			ID:    "sess-1",
			Title: "测试会话",
			Agent: "build",
			Model: &oc.ModelRef{ID: "glm-5-turbo", ProviderID: "zhipu"},
			Cost:  0.1234,
			Tokens: oc.SessionTokens{
				Input:  1000,
				Output: 2000,
			},
			Time: oc.SessionTime{Updated: now},
		},
		{
			ID:    "sess-2",
			Title: "未命名",
			Cost:  0,
			Tokens: oc.SessionTokens{
				Input:     500,
				Output:    1000,
				Reasoning: 3000,
				Cache:     oc.SessionCache{Read: 100, Write: 50},
			},
			Time: oc.SessionTime{Updated: now - 3600000}, // 1 hour ago
		},
	}
	agent := &fakeListAgent{sessions: sessions}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "", "/proj", "", "", "")

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if agent.lastDir != "/proj" {
		t.Errorf("ListSessions directory = %q, want /proj", agent.lastDir)
	}

	body := res.Body
	for _, want := range []string{"所有会话", "sess-1", "测试会话", "build", "zhipu/glm-5-turbo", "$0.1234", "3000", "sess-2", "未命名", "3000", "1小时前"} {
		if !contains(body, want) {
			t.Errorf("Body missing %q\ngot:\n%s", want, body)
		}
	}
}

// TestCmdListSessions_SortsByUpdated verifies sessions are sorted by updated time.
func TestCmdListSessions_SortsByUpdated(t *testing.T) {
	now := time.Now().UnixMilli()
	sessions := []oc.SessionInfo{
		{ID: "old", Title: "旧会话", Time: oc.SessionTime{Updated: now - 7200000}}, // 2 hours
		{ID: "new", Title: "新会话", Time: oc.SessionTime{Updated: now - 60000}},   // 1 min
		{ID: "mid", Title: "中会话", Time: oc.SessionTime{Updated: now - 3600000}}, // 1 hour
	}
	agent := &fakeListAgent{sessions: sessions}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "", "/proj", "", "", "")

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}

	body := res.Body
	// Verify order: new, mid, old
	newIdx := indexOf(body, "新会话")
	midIdx := indexOf(body, "中会话")
	oldIdx := indexOf(body, "旧会话")
	if newIdx < 0 || midIdx < 0 || oldIdx < 0 {
		t.Fatalf("missing session titles in body")
	}
	if newIdx >= midIdx || midIdx >= oldIdx {
		t.Errorf("sessions not sorted by updated time (newest first)\ngot order indices: new=%d mid=%d old=%d", newIdx, midIdx, oldIdx)
	}
}

// newHandlerWithAgent creates a Handler with the given fake agent and a fresh
// in-memory router for testing.
func newHandlerWithAgent(t *testing.T, agent opencodeAPI) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	return NewWithLogger(r, agent, nil, HandlerConfig{DefaultDirectory: t.TempDir()}, log.Nop()), r
}

// contains checks if a substring exists in a string (case-sensitive).
func contains(s, substr string) bool {
	return indexOf(s, substr) >= 0
}

// indexOf returns the index of a substring, or -1 if not found.
func indexOf(s, substr string) int {
	for i := range s {
		if len(s) >= i+len(substr) && s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
