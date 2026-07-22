package opencodeservebridge

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// useFakeAgent implements opencodeAPI for testing cmdSessionUse. The mutex
// guards statuses because the picker-recheck test flips a session busy while
// the picker goroutine is waiting for an answer.
type useFakeAgent struct {
	mu       sync.Mutex
	sessions []oc.SessionInfo
	statuses map[string]oc.SessionStatus
}

func (f *useFakeAgent) Run(context.Context, oc.RunOptions) (<-chan oc.HighEvent, error) {
	return nil, errors.New("not implemented")
}

func (f *useFakeAgent) ListModels(context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f *useFakeAgent) ListAgents(context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f *useFakeAgent) AbortSession(context.Context, string) error {
	return errors.New("not implemented")
}

func (f *useFakeAgent) ListSessions(context.Context, string) ([]oc.SessionInfo, error) {
	return f.sessions, nil
}

func (f *useFakeAgent) SessionStatuses(context.Context) (map[string]oc.SessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statuses, nil
}

func (f *useFakeAgent) DeleteSessionIfIdle(context.Context, string) error {
	return errors.New("not implemented")
}

func (f *useFakeAgent) ReplyPermission(context.Context, string, string, string, string) error {
	return errors.New("not implemented")
}

func (f *useFakeAgent) ReplyQuestion(context.Context, string, string, *oc.QuestionReply) error {
	return errors.New("not implemented")
}

func (f *useFakeAgent) RejectQuestion(context.Context, string, string) error {
	return errors.New("not implemented")
}

func (f *useFakeAgent) setStatus(id, typ string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[id] = oc.SessionStatus{Type: typ}
}

// useSessions returns two sessions whose sorted order is [s1, s2] (s1 newest).
func useSessions(now int64) []oc.SessionInfo {
	return []oc.SessionInfo{
		{ID: "s2", Title: "旧会话", Time: oc.SessionTime{Updated: now - 3600000}},
		{ID: "s1", Title: "新会话", Time: oc.SessionTime{Updated: now}},
	}
}

// TestCmdSessionUse_NoBinding verifies a chat with no binding gets the /cd
// hint and the command stays read-only (no lazy bind).
func TestCmdSessionUse_NoBinding(t *testing.T) {
	h, r := newHandlerWithAgent(t, &useFakeAgent{})

	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !contains(res.Body, "/cd") {
		t.Errorf("Body = %q, want /cd hint", res.Body)
	}
	if _, ok := r.Lookup("chat-1"); ok {
		t.Error("read-only guard must not create a binding")
	}
}

// TestCmdSessionUse_BindingWithoutDirectory verifies a binding lacking a
// working directory also gets the /cd hint.
func TestCmdSessionUse_BindingWithoutDirectory(t *testing.T) {
	h, r := newHandlerWithAgent(t, &useFakeAgent{})
	r.Bind("chat-1", "s1", "", "", "", "")

	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !contains(res.Body, "/cd") {
		t.Errorf("Body = %q, want /cd hint", res.Body)
	}
}

// TestCmdSessionUse_Empty verifies the direct path reports the empty state.
func TestCmdSessionUse_Empty(t *testing.T) {
	h, r := newHandlerWithAgent(t, &useFakeAgent{})
	r.Bind("chat-1", "", "/proj", "", "", "")

	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if res.Body != "当前目录下没有任何会话。" {
		t.Errorf("Body = %q, want '当前目录下没有任何会话。'", res.Body)
	}
}

// TestCmdSessionUse_IndexInvalid covers 序号 0、超范围与非数字：each is
// rejected with the valid range (or a not-a-number hint) and the binding is
// left untouched.
func TestCmdSessionUse_IndexInvalid(t *testing.T) {
	now := time.Now().UnixMilli()
	agent := &useFakeAgent{sessions: useSessions(now)}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/proj", "", "", "")

	for _, tc := range []struct {
		arg  string
		want string
	}{
		{"0", "有效范围 1-2"},
		{"3", "有效范围 1-2"},
		{"abc", "必须是数字"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{tc.arg})
			if err != nil {
				t.Fatalf("cmdSessionUse: %v", err)
			}
			if !contains(res.Body, tc.want) {
				t.Errorf("Body = %q, want %q", res.Body, tc.want)
			}
			b, _ := r.Lookup("chat-1")
			if b.SessionID != "s1" {
				t.Errorf("SessionID = %q, want s1 (rejection must not move the binding)", b.SessionID)
			}
		})
	}
}

// TestCmdSessionUse_DirectSwitch verifies /session-use 2 repoints the
// binding to the second sorted session while Directory/Model/Agent survive.
func TestCmdSessionUse_DirectSwitch(t *testing.T) {
	now := time.Now().UnixMilli()
	agent := &useFakeAgent{
		sessions: useSessions(now),
		statuses: map[string]oc.SessionStatus{},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/proj", "", "p/m", "build")

	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"2"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	for _, want := range []string{"已切换到会话「旧会话」", "/session-use", "/session-clean"} {
		if !contains(res.Body, want) {
			t.Errorf("Body = %q, missing %q", res.Body, want)
		}
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "s2" {
		t.Errorf("SessionID = %q, want s2", b.SessionID)
	}
	if b.Directory != "/proj" || b.ModelSpec != "p/m" || b.Agent != "build" {
		t.Errorf("binding = %+v, want Directory/Model/Agent preserved", b)
	}
}

// TestCmdSessionUse_DirectSwitch_BusyRejected verifies a busy target is
// refused on the direct path and the binding stays put.
func TestCmdSessionUse_DirectSwitch_BusyRejected(t *testing.T) {
	now := time.Now().UnixMilli()
	agent := &useFakeAgent{
		sessions: useSessions(now),
		statuses: map[string]oc.SessionStatus{"s2": {Type: "busy"}},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/proj", "", "", "")

	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"2"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !contains(res.Body, "正在执行") {
		t.Errorf("Body = %q, want busy refusal", res.Body)
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1 (busy refusal must not move the binding)", b.SessionID)
	}
}

// TestCmdSessionUse_AlreadyCurrent verifies switching to the bound session
// is a no-op.
func TestCmdSessionUse_AlreadyCurrent(t *testing.T) {
	now := time.Now().UnixMilli()
	agent := &useFakeAgent{
		sessions: useSessions(now),
		statuses: map[string]oc.SessionStatus{},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/proj", "", "", "")

	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !contains(res.Body, "已是当前会话") {
		t.Errorf("Body = %q, want '已是当前会话'", res.Body)
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "s1" {
		t.Errorf("SessionID = %q, want s1 (no-op)", b.SessionID)
	}
}

// TestSortedSessions verifies the shared ordering: most recently updated
// first, and the caller's slice is not mutated.
func TestSortedSessions(t *testing.T) {
	now := time.Now().UnixMilli()
	in := []oc.SessionInfo{
		{ID: "old", Time: oc.SessionTime{Updated: now - 7200000}},
		{ID: "new", Time: oc.SessionTime{Updated: now}},
		{ID: "mid", Time: oc.SessionTime{Updated: now - 3600000}},
	}
	sorted := sortedSessions(in)
	want := []string{"new", "mid", "old"}
	for i, id := range want {
		if sorted[i].ID != id {
			t.Errorf("sorted[%d].ID = %q, want %q", i, sorted[i].ID, id)
		}
	}
	if in[0].ID != "old" {
		t.Errorf("input mutated: in[0].ID = %q, want old", in[0].ID)
	}
}
