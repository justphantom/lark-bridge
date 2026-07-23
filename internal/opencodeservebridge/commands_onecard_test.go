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
