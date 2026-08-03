package claudebridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// sessionFakeAgent is a claudeAPI fake for /session-list, /session-clean and
// /use tests. It records every DeleteSession call (id + dir) behind a
// mutex so the test can assert ordering and counts after the async
// confirmation goroutines settle.
type sessionFakeAgent struct {
	mu       sync.Mutex
	sessions []claude.Session
	listErr  error
	deletes  []deleteCall
	delErr   map[string]error // sessionID -> error to return for that id
}

type deleteCall struct {
	dir string
	id  string
}

func (s *sessionFakeAgent) Run(context.Context, claude.RunOptions) (<-chan claude.Event, error) {
	ch := make(chan claude.Event)
	close(ch)
	return ch, nil
}
func (s *sessionFakeAgent) ListSettings(context.Context) ([]string, error) { return nil, nil }

func (s *sessionFakeAgent) ListSessions(context.Context, string) ([]claude.Session, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]claude.Session, len(s.sessions))
	copy(out, s.sessions)
	return out, nil
}

func (s *sessionFakeAgent) DeleteSession(_ context.Context, dir, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deletes = append(s.deletes, deleteCall{dir: dir, id: id})
	if s.delErr != nil {
		if err, ok := s.delErr[id]; ok {
			return err
		}
	}
	return nil
}

func (s *sessionFakeAgent) DeleteCalls() []deleteCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]deleteCall, len(s.deletes))
	copy(out, s.deletes)
	return out
}

// newSessionTestHandler builds a Handler with a session-capable fake agent.
// rpc stays nil so emits are no-ops; the test reads pending requestIDs
// straight from h.Answers to drive the confirmation flow.
func newSessionTestHandler(t *testing.T, agent claudeAPI) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, agent, nil, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{
			DefaultDirectory: t.TempDir(),
		},
	}, log.Nop())
	return h, r
}

// waitCond polls pred every 2ms until true or timeout.
func waitCond(timeout time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// --- /session-list ---

func TestCmdSessionList_NoBinding(t *testing.T) {
	h, _ := newSessionTestHandler(t, &sessionFakeAgent{})
	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !strings.Contains(res.Body, "当前群尚无会话") {
		t.Errorf("Body = %q, want contains '当前群尚无会话'", res.Body)
	}
}

func TestCmdSessionList_NoDirectory(t *testing.T) {
	h, r := newSessionTestHandler(t, &sessionFakeAgent{})
	r.Bind("chat-1", "", "", "", "", "")
	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !strings.Contains(res.Body, "尚未设置工作目录") {
		t.Errorf("Body = %q, want contains '尚未设置工作目录'", res.Body)
	}
}

func TestCmdSessionList_MarksCurrentAndSorts(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []claude.Session{
		{ID: "ses_a", Title: "Alpha", Updated: 2000},
		{ID: "ses_b", Title: "Beta", Updated: 1000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_a", "/tmp/proj", "", "", "")

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !strings.Contains(res.Body, "★") {
		t.Errorf("Body should mark the current session with ★: %q", res.Body)
	}
	// ses_a (Updated 2000) sorts before ses_b (Updated 1000).
	ia := strings.Index(res.Body, "ses_a")
	ib := strings.Index(res.Body, "ses_b")
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("expected ses_a before ses_b (newest first): ia=%d ib=%d\n%s", ia, ib, res.Body)
	}
}

func TestCmdSessionList_EmptyDir(t *testing.T) {
	agent := &sessionFakeAgent{sessions: nil}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "", "/tmp/proj", "", "", "")
	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !strings.Contains(res.Body, "当前目录下没有任何会话") {
		t.Errorf("Body = %q, want the empty-dir message", res.Body)
	}
}

func TestCmdSessionList_ListError(t *testing.T) {
	agent := &sessionFakeAgent{listErr: errors.New("fs error")}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "", "/tmp/proj", "", "", "")
	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !strings.Contains(res.Body, "获取会话列表失败") {
		t.Errorf("Body = %q, want the list-error message", res.Body)
	}
}

// --- /session-clean ---

func TestCmdSessionClean_NoBinding(t *testing.T) {
	h, _ := newSessionTestHandler(t, &sessionFakeAgent{})
	res, err := h.cmdSessionClean(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	if !strings.Contains(res.Body, "当前群尚无会话绑定") {
		t.Errorf("Body = %q", res.Body)
	}
}

func TestCmdSessionClean_NoDirectory(t *testing.T) {
	h, r := newSessionTestHandler(t, &sessionFakeAgent{})
	r.Bind("chat-1", "", "", "", "", "")
	res, err := h.cmdSessionClean(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	if !strings.Contains(res.Body, "尚未设置工作目录") {
		t.Errorf("Body = %q", res.Body)
	}
}

func TestCmdSessionClean_ExplicitID_ProtectsCurrent(t *testing.T) {
	agent := &sessionFakeAgent{}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_current", "/tmp/proj", "", "", "")

	res, err := h.cmdSessionClean(context.Background(), "chat-1", []string{"ses_current"})
	if err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	if !strings.Contains(res.Body, "不能删除当前绑定") {
		t.Errorf("Body = %q, want contains '不能删除当前绑定'", res.Body)
	}
	if calls := agent.DeleteCalls(); len(calls) != 0 {
		t.Errorf("expected no DeleteSession calls, got %v", calls)
	}
}

func TestCmdSessionClean_ExplicitID_ConfirmsThenDeletes(t *testing.T) {
	agent := &sessionFakeAgent{}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_keep", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionClean(context.Background(), "chat-1", []string{"ses_del"}); err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"confirm"}})

	if !waitCond(2*time.Second, func() bool { return len(agent.DeleteCalls()) == 1 }) {
		t.Fatalf("expected 1 delete, got %d", len(agent.DeleteCalls()))
	}
	c := agent.DeleteCalls()[0]
	if c.id != "ses_del" || c.dir != "/tmp/proj" {
		t.Errorf("delete call = %+v, want {/tmp/proj, ses_del}", c)
	}
}

func TestCmdSessionClean_ExplicitID_Cancelled(t *testing.T) {
	agent := &sessionFakeAgent{}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_keep", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionClean(context.Background(), "chat-1", []string{"ses_del"}); err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"cancel"}})

	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) == 0 }) {
		t.Fatal("cancel answer not processed")
	}
	if calls := agent.DeleteCalls(); len(calls) != 0 {
		t.Errorf("cancel should perform no deletes, got %v", calls)
	}
}

func TestCmdSessionClean_Batch_FiltersCurrentAndConfirms(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []claude.Session{
		{ID: "ses_keep", Title: "current", Updated: 3000},
		{ID: "ses_a", Title: "A", Updated: 2000},
		{ID: "ses_b", Title: "B", Updated: 1000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_keep", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionClean(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"confirm"}})

	if !waitCond(2*time.Second, func() bool { return len(agent.DeleteCalls()) == 2 }) {
		t.Fatalf("expected 2 deletes (current excluded), got %d", len(agent.DeleteCalls()))
	}
	deleted := map[string]bool{}
	for _, c := range agent.DeleteCalls() {
		if c.dir != "/tmp/proj" {
			t.Errorf("delete dir = %q, want /tmp/proj", c.dir)
		}
		deleted[c.id] = true
	}
	if deleted["ses_keep"] {
		t.Errorf("current session must NOT be deleted")
	}
	if !deleted["ses_a"] || !deleted["ses_b"] {
		t.Errorf("expected ses_a and ses_b deleted, got %v", deleted)
	}
}

func TestCmdSessionClean_Batch_NothingToDelete(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []claude.Session{
		{ID: "ses_only", Title: "the one", Updated: 1000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_only", "/tmp/proj", "", "", "")

	res, err := h.cmdSessionClean(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	// Nothing-to-clean is a synchronous message — no confirmation card.
	if !strings.Contains(res.Body, "没有可清理的会话") {
		t.Errorf("Body = %q, want the nothing-to-clean message", res.Body)
	}
	if calls := agent.DeleteCalls(); len(calls) != 0 {
		t.Errorf("expected zero deletes, got %v", calls)
	}
}

// --- /use ---

func TestCmdSessionUse_NoBinding(t *testing.T) {
	h, _ := newSessionTestHandler(t, &sessionFakeAgent{})
	res, err := h.cmdSessionUse(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !strings.Contains(res.Body, "当前群尚无会话") {
		t.Errorf("Body = %q", res.Body)
	}
}

func TestCmdSessionUse_NoDirectory(t *testing.T) {
	h, r := newSessionTestHandler(t, &sessionFakeAgent{})
	r.Bind("chat-1", "", "", "", "", "")
	res, err := h.cmdSessionUse(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !strings.Contains(res.Body, "尚未设置工作目录") {
		t.Errorf("Body = %q", res.Body)
	}
}

func TestCmdSessionUse_InvalidNumber(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []claude.Session{{ID: "ses_x", Title: "X", Updated: 1000}}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_x", "/tmp/proj", "", "", "")
	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"abc"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !strings.Contains(res.Body, "会话序号必须是数字") {
		t.Errorf("Body = %q, want the invalid-number message", res.Body)
	}
}

func TestCmdSessionUse_NumberOutOfRange(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []claude.Session{{ID: "ses_x", Title: "X", Updated: 1000}}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_x", "/tmp/proj", "", "", "")
	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"9"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !strings.Contains(res.Body, "越界") {
		t.Errorf("Body = %q, want the out-of-range message", res.Body)
	}
}

func TestCmdSessionUse_ByIndexSwitches(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []claude.Session{
		{ID: "ses_old", Title: "Old", Updated: 1000},
		{ID: "ses_new", Title: "New", Updated: 2000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_old", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"}); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	// sorted[0] is ses_new (Updated 2000 > 1000); index 1 → ses_new.
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "ses_new" {
		t.Errorf("SessionID = %q, want ses_new (sorted[0])", b.SessionID)
	}
}

func TestCmdSessionUse_AlreadyCurrentNoop(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []claude.Session{
		{ID: "ses_only", Title: "Only", Updated: 1000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_only", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"}); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "ses_only" {
		t.Errorf("SessionID = %q, want unchanged ses_only", b.SessionID)
	}
	if calls := agent.DeleteCalls(); len(calls) != 0 {
		t.Errorf("switching to current must not delete anything, got %v", calls)
	}
}

func TestCmdSessionUse_Picker_ConfirmsAndSwitches(t *testing.T) {
	recent := time.Now().Add(-20 * time.Second).UnixMilli()
	agent := &sessionFakeAgent{sessions: []claude.Session{
		{ID: "ses_old", Title: "Old", Updated: recent},
		{ID: "ses_new", Title: "New", Updated: recent + 1000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_old", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	reqID := waitPending(t, h, time.Second)
	// The picker lists sorted sessions; deliver the label of sorted[0] (ses_new).
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"1. New · 刚刚"}})

	if !waitCond(time.Second, func() bool {
		b, _ := r.Lookup("chat-1")
		return b.SessionID == "ses_new"
	}) {
		b, _ := r.Lookup("chat-1")
		t.Fatalf("SessionID = %q, want ses_new after picker confirm", b.SessionID)
	}
}
