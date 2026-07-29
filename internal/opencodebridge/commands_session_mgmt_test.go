package opencodebridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencode"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// recordingSessionAgent extends sessionFakeAgent with a counter so tests can
// wait on the list goroutine without sleeping a fixed interval.
type recordingSessionAgent struct {
	sessionFakeAgent

	listCalls atomic.Int32
}

func (r *recordingSessionAgent) ListSessions(ctx context.Context, dir string) ([]opencode.Session, error) {
	r.listCalls.Add(1)
	return r.sessionFakeAgent.ListSessions(ctx, dir)
}

// sessionFakeAgent is an opencodeAPI fake for /session-list and /session-clean
// tests. It records every DeleteSession call (id + dir) behind a mutex so the
// test can assert ordering and counts after the async goroutines settle.
type sessionFakeAgent struct {
	mu       sync.Mutex
	sessions []opencode.Session
	listErr  error
	deletes  []deleteCall
	delErr   map[string]error // sessionID -> error to return for that id
}

type deleteCall struct {
	dir string
	id  string
}

func (s *sessionFakeAgent) Run(context.Context, opencode.RunOptions) (<-chan opencode.Event, error) {
	ch := make(chan opencode.Event)
	close(ch)
	return ch, nil
}

func (s *sessionFakeAgent) ListModels(context.Context) ([]string, error) { return nil, nil }
func (s *sessionFakeAgent) ListAgents(context.Context) ([]string, error) { return nil, nil }

func (s *sessionFakeAgent) ListSessions(context.Context, string) ([]opencode.Session, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]opencode.Session, len(s.sessions))
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
func newSessionTestHandler(t *testing.T, agent opencodeAPI) (*Handler, *router.Router) {
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

// TestCmdSessionList_NoBinding verifies /session-list on a chat with no
// binding surfaces the "no session" message and stays synchronous (no async
// goroutine, no pending slot).
func TestCmdSessionList_NoBinding(t *testing.T) {
	h, _ := newSessionTestHandler(t, &sessionFakeAgent{})
	res, err := h.cmdSessionList(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionList: %v", err)
	}
	if !strings.Contains(res.Body, "当前群尚无会话") {
		t.Errorf("Body = %q, want contains '当前群尚无会话'", res.Body)
	}
	if len(h.Answers.PendingIDs()) != 0 {
		t.Error("expected no pending slot on the no-binding path")
	}
}

// TestCmdSessionList_NoDirectory verifies /session-list on a chat whose
// binding has no directory pin asks the user to /cd first.
func TestCmdSessionList_NoDirectory(t *testing.T) {
	h, r := newSessionTestHandler(t, &sessionFakeAgent{})
	r.Bind("chat-1", "", "", "", "", "")
	res, err := h.cmdSessionList(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionList: %v", err)
	}
	if !strings.Contains(res.Body, "尚未设置工作目录") {
		t.Errorf("Body = %q, want contains '尚未设置工作目录'", res.Body)
	}
}

// TestCmdSessionList_AsyncInvokesList verifies the happy path: command
// returns Handled immediately, the goroutine calls ListSessions exactly once
// with the binding's directory, and no pending slot lingers (list never
// registers a Question card).
func TestCmdSessionList_AsyncInvokesList(t *testing.T) {
	agent := &recordingSessionAgent{
		sessionFakeAgent: sessionFakeAgent{
			sessions: []opencode.Session{
				{ID: "ses_a", Title: "Alpha", Updated: 2000, Created: 1000},
				{ID: "ses_b", Title: "Beta", Updated: 1000, Created: 500},
			},
		},
	}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_a", "/tmp/proj", "", "", "")

	res, err := h.cmdSessionList(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionList: %v", err)
	}
	if !res.Handled {
		t.Error("expected Handled=true for the async path")
	}
	if !waitCond(time.Second, func() bool { return agent.listCalls.Load() == 1 }) {
		t.Fatalf("ListSessions not invoked; calls=%d", agent.listCalls.Load())
	}
	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) == 0 }) {
		t.Error("expected no pending slot after list completes")
	}
}

// TestCmdSessionList_ListError verifies a failing ListRegisters no pending
// slot. The error Notice is emitted on a nil rpc (no-op); the assertion is
// the goroutine terminates cleanly.
func TestCmdSessionList_ListError(t *testing.T) {
	agent := &recordingSessionAgent{
		sessionFakeAgent: sessionFakeAgent{
			listErr: errors.New("provider offline"),
		},
	}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionList(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionList should return nil error (async): %v", err)
	}
	if !waitCond(time.Second, func() bool { return agent.listCalls.Load() == 1 }) {
		t.Fatal("ListSessions not invoked")
	}
	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) == 0 }) {
		t.Error("expected no lingering slot after list error")
	}
}

// --- /session-clean ---

// TestCmdSessionClean_NoBinding verifies the synchronous no-binding message.
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

// TestCmdSessionClean_NoDirectory verifies the synchronous no-directory
// message.
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

// TestCmdSessionClean_ExplicitID_ProtectsCurrent verifies that passing the
// currently-bound session id is rejected synchronously with a clear message,
// rather than going through confirmation + delete of the active conversation.
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

// TestCmdSessionClean_ExplicitID_ConfirmsThenDeletes verifies the explicit-id
// path: AskPermission pops a confirmation card, the user picks 确认, and
// exactly one DeleteSession call lands with the right dir + id.
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

// TestCmdSessionClean_ExplicitID_Cancelled verifies choosing 取消 in the
// confirmation card performs zero deletes.
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

// TestCmdSessionClean_Batch_FiltersCurrentAndConfirms verifies the no-args
// path: list, drop the current session, ask for confirmation, and delete
// exactly the remaining ids. The bound session MUST NOT be among the deletes.
func TestCmdSessionClean_Batch_FiltersCurrentAndConfirms(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []opencode.Session{
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
		t.Error("current session was deleted; it must be protected")
	}
	if !deleted["ses_a"] || !deleted["ses_b"] {
		t.Errorf("expected ses_a + ses_b deleted, got %v", deleted)
	}
}

// TestCmdSessionClean_Batch_NothingToDelete verifies that when the list
// contains only the current session, no confirmation card pops (nothing to
// clean) and zero deletes run.
func TestCmdSessionClean_Batch_NothingToDelete(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []opencode.Session{
		{ID: "ses_only", Title: "the one", Updated: 1000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_only", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionClean(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	if !waitCond(time.Second, func() bool {
		return len(h.Answers.PendingIDs()) == 0
	}) {
		t.Fatal("expected no confirmation card for nothing-to-clean")
	}
	if calls := agent.DeleteCalls(); len(calls) != 0 {
		t.Errorf("expected zero deletes, got %v", calls)
	}
}

// TestCmdSessionClean_Batch_PartialFailure verifies a failing delete is
// counted as such while successful ones still go through; the bridge does not
// abort the loop on the first error.
func TestCmdSessionClean_Batch_PartialFailure(t *testing.T) {
	agent := &sessionFakeAgent{
		sessions: []opencode.Session{
			{ID: "ses_ok", Title: "ok", Updated: 2000},
			{ID: "ses_bad", Title: "bad", Updated: 1000},
		},
		delErr: map[string]error{"ses_bad": errors.New("Session not found: ses_bad")},
	}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_keep", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionClean(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionClean: %v", err)
	}
	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"confirm"}})

	if !waitCond(2*time.Second, func() bool { return len(agent.DeleteCalls()) == 2 }) {
		t.Fatalf("expected both ids attempted, got %d", len(agent.DeleteCalls()))
	}
}

// --- /session-use ---

// TestCmdSessionUse_NoBinding verifies the synchronous no-binding message.
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

// TestCmdSessionUse_NoDirectory verifies the synchronous no-directory message.
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

// TestCmdSessionUse_InvalidNumber verifies a non-numeric arg surfaces a
// synchronous hint instead of forking the CLI.
func TestCmdSessionUse_InvalidNumber(t *testing.T) {
	h, r := newSessionTestHandler(t, &sessionFakeAgent{})
	r.Bind("chat-1", "ses_x", "/tmp/proj", "", "", "")
	res, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"abc"})
	if err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !strings.Contains(res.Body, "会话序号必须是数字") {
		t.Errorf("Body = %q", res.Body)
	}
}

// TestCmdSessionUse_NumberOutOfRange verifies an out-of-range index emits an
// error Notice after the (fake) list returns, with no binding change.
func TestCmdSessionUse_NumberOutOfRange(t *testing.T) {
	agent := &recordingSessionAgent{
		sessionFakeAgent: sessionFakeAgent{
			sessions: []opencode.Session{
				{ID: "ses_a", Title: "A", Updated: 1000},
			},
		},
	}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_a", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"5"}); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !waitCond(time.Second, func() bool { return agent.listCalls.Load() >= 1 }) {
		t.Fatal("ListSessions not invoked")
	}
	// Give the goroutine a moment to emit the range error.
	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) == 0 }) {
		t.Fatal("expected no pending slot after range error")
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "ses_a" {
		t.Errorf("SessionID = %q, binding must be unchanged", b.SessionID)
	}
}

// TestCmdSessionUse_ByIndexSwitches verifies /session-use <n> repoints the
// binding to the n-th session of the sorted list and aborts the in-flight
// turn first.
func TestCmdSessionUse_ByIndexSwitches(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []opencode.Session{
		{ID: "ses_old", Title: "Old", Updated: 1000},
		{ID: "ses_new", Title: "New", Updated: 2000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_old", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"}); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	if !waitCond(time.Second, func() bool {
		b, _ := r.Lookup("chat-1")
		return b.SessionID == "ses_new"
	}) {
		b, _ := r.Lookup("chat-1")
		t.Fatalf("SessionID = %q, want ses_new (sorted[0])", b.SessionID)
	}
}

// TestCmdSessionUse_AlreadyCurrentNoop verifies switching to the currently
// bound session is a no-op: no AbortChat, no SetSessionID, no log entry.
func TestCmdSessionUse_AlreadyCurrentNoop(t *testing.T) {
	agent := &sessionFakeAgent{sessions: []opencode.Session{
		{ID: "ses_only", Title: "Only", Updated: 1000},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_only", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", []string{"1"}); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	// Wait for the goroutine to land the no-op notice.
	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) == 0 }) {
		t.Fatal("expected goroutine to terminate after no-op switch")
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "ses_only" {
		t.Errorf("SessionID = %q, must be unchanged", b.SessionID)
	}
}

// TestCmdSessionUse_Picker_ConfirmsAndSwitches verifies the no-args picker
// path: AskAndWait pops a Question card, the user picks an option, and the
// binding repoints to the matching session. Uses a fixed recent timestamp so
// formatSessionTime yields a predictable "刚刚" suffix.
func TestCmdSessionUse_Picker_ConfirmsAndSwitches(t *testing.T) {
	recent := time.Now().Add(-20 * time.Second).UnixMilli()
	agent := &sessionFakeAgent{sessions: []opencode.Session{
		{ID: "ses_old", Title: "Old", Updated: recent},
		{ID: "ses_new", Title: "New", Updated: recent},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_old", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	waitPending(t, h, time.Second)
	pending := h.Answers.PendingIDs()
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending slot, got %v", pending)
	}
	// runSessionUsePicker sorts sessions (stable on ties, so input order
	// preserved) and labels them "<n>. <title> · <time>", prefixing the
	// currently-bound one with "★ ". With Old first and current:
	//   options[0] = "★ 1. Old · 刚刚"
	//   options[1] = "2. New · 刚刚"
	// Delivering the non-current option repoints the binding to ses_new.
	h.Answers.Deliver(pending[0], &protocol.AnswerPayload{Choices: []string{"2. New · 刚刚"}})

	if !waitCond(2*time.Second, func() bool {
		b, _ := r.Lookup("chat-1")
		return b.SessionID == "ses_new"
	}) {
		b, _ := r.Lookup("chat-1")
		t.Fatalf("SessionID = %q, want ses_new after picker confirm", b.SessionID)
	}
}

// TestCmdSessionUse_Picker_CurrentIsNoop verifies picking the currently-bound
// session's option is a no-op (binding unchanged).
func TestCmdSessionUse_Picker_CurrentIsNoop(t *testing.T) {
	recent := time.Now().Add(-20 * time.Second).UnixMilli()
	agent := &sessionFakeAgent{sessions: []opencode.Session{
		{ID: "ses_only", Title: "Only", Updated: recent},
	}}
	h, r := newSessionTestHandler(t, agent)
	r.Bind("chat-1", "ses_only", "/tmp/proj", "", "", "")

	if _, err := h.cmdSessionUse(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionUse: %v", err)
	}
	waitPending(t, h, time.Second)
	pending := h.Answers.PendingIDs()
	h.Answers.Deliver(pending[0], &protocol.AnswerPayload{Choices: []string{"★ 1. Only · 刚刚"}})

	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) == 0 }) {
		t.Fatal("picker did not terminate after no-op answer")
	}
	b, _ := r.Lookup("chat-1")
	if b.SessionID != "ses_only" {
		t.Errorf("SessionID = %q, must be unchanged", b.SessionID)
	}
}

// --- formatSessionList / summarizeClean / formatSessionTime ---

func TestFormatSessionList_SortedAndMarksCurrent(t *testing.T) {
	sessions := []opencode.Session{
		{ID: "ses_old", Title: "Old", Updated: 1000},
		{ID: "ses_cur", Title: "Cur", Updated: 3000},
		{ID: "ses_mid", Title: "Mid", Updated: 2000},
	}
	got := formatSessionList(sessions, "ses_cur")
	// Current must come first (highest Updated) and carry the star marker.
	if !strings.HasPrefix(got, "📋 目录下会话（3）\n\n★ 1. Cur\n") {
		t.Errorf("expected current session first with star, got:\n%s", got)
	}
	if !strings.Contains(got, "2. Mid") || !strings.Contains(got, "3. Old") {
		t.Errorf("expected Mid and Old after Cur, got:\n%s", got)
	}
}

func TestFormatSessionList_Empty(t *testing.T) {
	if got := formatSessionList(nil, ""); !strings.Contains(got, "没有任何会话") {
		t.Errorf("empty list body = %q", got)
	}
}

func TestSummarizeClean(t *testing.T) {
	cases := []struct {
		name   string
		count  int
		failed []string
		want   string
		level  string
	}{
		{"all succeed", 3, nil, "已删除 3 个会话。", "success"},
		{"partial fail", 3, []string{"ses_x"}, "已删除 2/3 个会话；失败 1 个：ses_x。", "warning"},
		{"all fail", 2, []string{"ses_a", "ses_b"}, "全部 2 个会话删除失败（可能已不存在或被外部修改）。", "warning"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, level := summarizeClean(c.count, c.failed)
			if body != c.want {
				t.Errorf("body = %q, want %q", body, c.want)
			}
			if level != c.level {
				t.Errorf("level = %q, want %q", level, c.level)
			}
		})
	}
}

func TestFormatSessionTime(t *testing.T) {
	cases := []struct {
		name string
		ms   int64
		want string
	}{
		{"zero is unknown", 0, "(未知)"},
		{"just now", time.Now().Add(20 * time.Second).UnixMilli(), "刚刚"},
		{"minutes ago", time.Now().Add(-5 * time.Minute).UnixMilli(), "5分钟前"},
		{"one hour ago", time.Now().Add(-1 * time.Hour).UnixMilli(), "1小时前"},
		{"hours ago", time.Now().Add(-3 * time.Hour).UnixMilli(), "3小时前"},
		{"one day ago", time.Now().Add(-26 * time.Hour).UnixMilli(), "1天前"},
		{"days ago", time.Now().Add(-72 * time.Hour).UnixMilli(), "3天前"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := formatSessionTime(c.ms)
			if got != c.want {
				t.Errorf("formatSessionTime(%d) = %q, want %q", c.ms, got, c.want)
			}
		})
	}
	// Old timestamp falls back to the absolute date format; assert the
	// shape rather than the exact date so the test is timezone-stable.
	old := time.Now().Add(-30 * 24 * time.Hour).UnixMilli()
	got := formatSessionTime(old)
	if len(got) != len("2006-01-02 15:04") {
		t.Errorf("old timestamp format = %q, want YYYY-MM-DD HH:MM shape", got)
	}
}
