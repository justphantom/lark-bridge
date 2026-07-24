package opencodeservebridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// controlCapture is a fake frontend: the SSE handshake hangs forever (no
// events), and every POSTed control is decoded and recorded so tests can
// assert what the bridge emitted.
type controlCapture struct {
	mu    sync.Mutex
	ctrls []*protocol.Control
}

func (c *controlCapture) add(ctrl *protocol.Control) {
	c.mu.Lock()
	c.ctrls = append(c.ctrls, ctrl)
	c.mu.Unlock()
}

// find returns the first recorded control matching pred, or nil.
func (c *controlCapture) find(pred func(*protocol.Control) bool) *protocol.Control {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ctrl := range c.ctrls {
		if pred(ctrl) {
			return ctrl
		}
	}
	return nil
}

// waitFor polls until a control matching pred arrives or the timeout elapses.
func (c *controlCapture) waitFor(t *testing.T, pred func(*protocol.Control) bool, timeout time.Duration) *protocol.Control {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctrl := c.find(pred); ctrl != nil {
			return ctrl
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no matching control arrived within timeout")
	return nil
}

// newWireHandler builds a Handler whose IPC client points at a fake frontend
// that records every emitted control.
func newWireHandler(t *testing.T, agent opencodeAPI) (*Handler, *router.Router, *controlCapture) {
	t.Helper()
	captured := &controlCapture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	mux.HandleFunc("/v1/control/", func(w http.ResponseWriter, r *http.Request) {
		var ctrl protocol.Control
		if err := json.NewDecoder(r.Body).Decode(&ctrl); err == nil {
			captured.add(&ctrl)
		}
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	rpc, err := backendrpc.Connect("opencode-serve-t", "opencode", ts.URL, "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = rpc.Close() })

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, agent, rpc, HandlerConfig{DefaultDirectory: t.TempDir()}, log.Nop())
	t.Cleanup(func() { h.Close() })
	return h, r, captured
}

func isQuestion(ctrl *protocol.Control) bool { return ctrl.Type == protocol.TypeQuestion }

// TestCmdModel_Picker_OneCardFlow pins the single-card contract for /model:
// the loading delta and the picker card both address the command message's
// promptID (so the frontend morphs its progress card), no standalone
// placeholder notice is emitted, and the result patches the answered card via
// UpdateMessageID.
func TestCmdModel_Picker_OneCardFlow(t *testing.T) {
	h, _, captured := newWireHandler(t, pickerFakeAgent{models: []string{"p/a", "p/b"}})

	commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/model", "om_cmd")

	q := captured.waitFor(t, isQuestion, 2*time.Second)
	if q.PromptID != "om_cmd" {
		t.Errorf("question promptID = %q, want om_cmd", q.PromptID)
	}
	if !q.Question.TakeOverProgress {
		t.Error("question should request progress-card takeover")
	}
	delta := captured.find(func(c *protocol.Control) bool {
		return c.Type == protocol.TypeText && c.PromptID == "om_cmd"
	})
	if delta == nil {
		t.Error("loading delta on the command's promptID not emitted")
	}
	placeholder := captured.find(func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "正在加载模型列表"
	})
	if placeholder != nil {
		t.Error("standalone placeholder notice should be gone")
	}

	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1", MessageID: "om_progress", Choices: []string{"p/b"},
	})
	res := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "已切换模型"
	}, 2*time.Second)
	if res.Notice.UpdateMessageID != "om_progress" {
		t.Errorf("result UpdateMessageID = %q, want om_progress", res.Notice.UpdateMessageID)
	}
}

// TestCmdModel_Picker_ListErrorOneCard verifies a picker failure terminates
// the command's progress card in place (notice bound to its promptID) instead
// of leaving the "处理中" placeholder hanging next to a standalone card.
func TestCmdModel_Picker_ListErrorOneCard(t *testing.T) {
	h, _, captured := newWireHandler(t, failingListAgent{})

	commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/model", "om_cmd")

	n := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Level == "error"
	}, 2*time.Second)
	if n.PromptID != "om_cmd" {
		t.Errorf("error notice promptID = %q, want om_cmd", n.PromptID)
	}
	if n.Notice.Title != "选择失败" {
		t.Errorf("error notice title = %q, want 选择失败", n.Notice.Title)
	}
}

// TestCmdDirectory_Picker_OneCardFlow pins the same contract for /cd: the
// picker card addresses the command's promptID and the result patches the
// answered card.
func TestCmdDirectory_Picker_OneCardFlow(t *testing.T) {
	h, _, captured := newWireHandler(t, pickerFakeAgent{})
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.DirCache = bridgebase.NewDirCache(workspace)

	// /cd's picker blocks inside Dispatch until the answer arrives.
	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/cd", "om_cmd")

	q := captured.waitFor(t, isQuestion, 2*time.Second)
	if q.PromptID != "om_cmd" {
		t.Errorf("question promptID = %q, want om_cmd", q.PromptID)
	}
	if !q.Question.TakeOverProgress {
		t.Error("question should request progress-card takeover")
	}

	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1", MessageID: "om_progress", Choices: []string{"proj"},
	})
	res := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "已切换目录"
	}, 2*time.Second)
	if res.Notice.UpdateMessageID != "om_progress" {
		t.Errorf("result UpdateMessageID = %q, want om_progress", res.Notice.UpdateMessageID)
	}
}

// TestCmdSessionUse_Picker_OneCardFlow pins the single-card contract for
// /session-use: the loading delta and the picker card both address the
// command message's promptID (so the frontend morphs its progress card), no
// standalone placeholder notice is emitted, and the result patches the
// answered card via UpdateMessageID.
func TestCmdSessionUse_Picker_OneCardFlow(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{
			"/a": {{ID: "s1", Title: "会话一"}, {ID: "s2", Title: "会话二"}},
		},
		statuses: map[string]oc.SessionStatus{
			"s1": {Type: "idle"},
			"s2": {Type: "idle"},
		},
	}
	h, r, captured := newWireHandler(t, agent)
	r.Bind("chat-1", "", "/a", "", "", "")

	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/session-use", "ou_cmd")

	q := captured.waitFor(t, isQuestion, 2*time.Second)
	if q.PromptID != "ou_cmd" {
		t.Errorf("question promptID = %q, want ou_cmd", q.PromptID)
	}
	if !q.Question.TakeOverProgress {
		t.Error("question should request progress-card takeover")
	}
	if delta := captured.find(func(c *protocol.Control) bool {
		return c.Type == protocol.TypeText && c.PromptID == "ou_cmd"
	}); delta == nil {
		t.Error("loading delta on the command's promptID not emitted")
	}
	if placeholder := captured.find(func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "正在加载会话列表"
	}); placeholder != nil {
		t.Error("standalone placeholder notice should be gone")
	}

	// Reuse the exact label the picker generated (time-stamped) so the
	// choice maps back to a real session regardless of formatTime output.
	firstLabel := q.Question.Questions[0].Options[0]
	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1", MessageID: "ou_progress",
		Choices: []string{firstLabel},
	})
	res := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "已切换会话"
	}, 2*time.Second)
	if res.Notice.UpdateMessageID != "ou_progress" {
		t.Errorf("result UpdateMessageID = %q, want ou_progress", res.Notice.UpdateMessageID)
	}
}

// TestCmdSessionUse_Picker_NoSessions_BindToProgress verifies the
// no-sessions terminal binds back to the command's progress card instead of
// popping a standalone card, and never emits a picker question.
func TestCmdSessionUse_Picker_NoSessions_BindToProgress(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{"/a": nil},
	}
	h, r, captured := newWireHandler(t, agent)
	r.Bind("chat-1", "", "/a", "", "", "")

	commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/session-use", "ou_cmd")

	n := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "无会话"
	}, 2*time.Second)
	if n.PromptID != "ou_cmd" {
		t.Errorf("no-sessions notice promptID = %q, want ou_cmd", n.PromptID)
	}
	if captured.find(isQuestion) != nil {
		t.Error("no-sessions terminal should not pop a question card")
	}
}

// TestCmdSessionClean_Picker_OneCardFlow pins the single-card contract for
// /session-clean: the confirmation card addresses the command message's
// promptID (TakeOverProgress, no standalone placeholder), and the result
// patches the answered card via UpdateMessageID.
func TestCmdSessionClean_Picker_OneCardFlow(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{
			"/a": {{ID: "idle1"}, {ID: "bound"}},
		},
		statuses: map[string]oc.SessionStatus{
			"idle1": {Type: "idle"},
			"bound": {Type: "idle"},
		},
	}
	h, r, captured := newWireHandler(t, agent)
	r.Bind("chat-1", "bound", "/a", "", "", "") // "bound" is bound → not a candidate

	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/session-clean", "oc_cmd")

	q := captured.waitFor(t, isQuestion, 2*time.Second)
	if q.PromptID != "oc_cmd" {
		t.Errorf("question promptID = %q, want oc_cmd", q.PromptID)
	}
	if !q.Question.TakeOverProgress {
		t.Error("question should request progress-card takeover")
	}
	if placeholder := captured.find(func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "等待确认清理"
	}); placeholder != nil {
		t.Error("standalone placeholder notice should be gone")
	}

	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1", MessageID: "oc_progress",
		Choices: []string{"确认清理"},
	})
	res := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "已清理空闲会话"
	}, 2*time.Second)
	if res.Notice.UpdateMessageID != "oc_progress" {
		t.Errorf("result UpdateMessageID = %q, want oc_progress", res.Notice.UpdateMessageID)
	}
}

// TestCmdSessionClean_Cancel_BindToProgress verifies the cancel terminal
// patches the answered picker card in place and deletes nothing.
func TestCmdSessionClean_Cancel_BindToProgress(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{"/a": {{ID: "idle1"}}},
		statuses:      map[string]oc.SessionStatus{"idle1": {Type: "idle"}},
	}
	h, r, captured := newWireHandler(t, agent)
	r.Bind("chat-1", "", "/a", "", "", "")

	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/session-clean", "oc_cmd")

	q := captured.waitFor(t, isQuestion, 2*time.Second)
	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1", MessageID: "oc_progress",
		Choices: []string{"取消"},
	})
	res := captured.waitFor(t, func(c *protocol.Control) bool {
		return c.Type == protocol.TypeNotice && c.Notice != nil && c.Notice.Title == "已取消清理"
	}, 2*time.Second)
	if res.Notice.UpdateMessageID != "oc_progress" {
		t.Errorf("cancel UpdateMessageID = %q, want oc_progress", res.Notice.UpdateMessageID)
	}
	if got := agent.deletedSnapshot(); len(got) != 0 {
		t.Errorf("deleted = %v, want none after 取消", got)
	}
}
