package opencodeservebridge

import (
	"context"
	"sync"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// replyCall records one serve reply invocation: kind is "permission" /
// "question" / "reject", requestID the serve-side request id.
type replyCall struct {
	kind      string
	requestID string
	reply     string
	answers   [][]string
}

// recordingAgent captures ReplyPermission/ReplyQuestion/RejectQuestion calls
// so interactive tests can assert what was sent back to the serve server.
type recordingAgent struct {
	closedStreamOpencode

	mu    sync.Mutex
	calls []replyCall
	// signal receives one empty struct per reply call for synchronisation.
	signal chan struct{}
}

func newRecordingAgent() *recordingAgent {
	return &recordingAgent{signal: make(chan struct{}, 8)}
}

func (a *recordingAgent) notify() {
	select {
	case a.signal <- struct{}{}:
	default:
	}
}

func (a *recordingAgent) ReplyPermission(_ context.Context, requestID, reply, _ string) error {
	a.mu.Lock()
	a.calls = append(a.calls, replyCall{kind: "permission", requestID: requestID, reply: reply})
	a.mu.Unlock()
	a.notify()
	return nil
}

func (a *recordingAgent) ReplyQuestion(_ context.Context, requestID string, r *oc.QuestionReply) error {
	a.mu.Lock()
	a.calls = append(a.calls, replyCall{kind: "question", requestID: requestID, answers: r.Answers})
	a.mu.Unlock()
	a.notify()
	return nil
}

func (a *recordingAgent) RejectQuestion(_ context.Context, requestID string) error {
	a.mu.Lock()
	a.calls = append(a.calls, replyCall{kind: "reject", requestID: requestID})
	a.mu.Unlock()
	a.notify()
	return nil
}

// waitCall blocks until the agent records one reply call and returns it.
func waitCall(t *testing.T, a *recordingAgent) replyCall {
	t.Helper()
	select {
	case <-a.signal:
	case <-time.After(2 * time.Second):
		t.Fatal("no serve reply recorded within timeout")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls[len(a.calls)-1]
}

func TestPermissionReplyOf(t *testing.T) {
	cases := []struct{ label, want string }{
		{"允许一次", oc.PermissionReplyOnce},
		{"始终允许", oc.PermissionReplyAlways},
		{"拒绝", oc.PermissionReplyReject},
		{"", oc.PermissionReplyReject},
		{"未知的选项", oc.PermissionReplyReject},
	}
	for _, c := range cases {
		if got := permissionReplyOf(c.label); got != c.want {
			t.Errorf("permissionReplyOf(%q) = %q, want %q", c.label, got, c.want)
		}
	}
}

// TestHandlePermissionAsked_RepliesChoice drives the full loop: asked event →
// pending answer slot → user picks 允许一次 → serve gets reply "once".
func TestHandlePermissionAsked_RepliesChoice(t *testing.T) {
	agent := newRecordingAgent()
	h, _ := newPickerHandlerWithAgent(t, agent)
	p := &oc.PermissionAskedData{ID: "perm-1", SessionID: "s1", Permission: "bash", Patterns: []string{"make test"}}

	go h.handlePermissionAsked(context.Background(), "c1", "p1", p)
	if rid := waitPending(t, h, 2*time.Second); rid != "perm-1" {
		t.Fatalf("pending request id = %q, want perm-1", rid)
	}
	h.Answers.Deliver("perm-1", &protocol.AnswerPayload{Choices: []string{"允许一次"}})

	call := waitCall(t, agent)
	if call.kind != "permission" || call.requestID != "perm-1" || call.reply != oc.PermissionReplyOnce {
		t.Errorf("call = %+v, want permission perm-1 once", call)
	}
}

// TestHandlePermissionAsked_AbortRejects pins the safety rule: a prompt
// cancel while the user has not answered must reject the permission, or the
// serve-side agent hangs forever.
func TestHandlePermissionAsked_AbortRejects(t *testing.T) {
	agent := newRecordingAgent()
	h, _ := newPickerHandlerWithAgent(t, agent)
	p := &oc.PermissionAskedData{ID: "perm-2", SessionID: "s1", Permission: "bash"}

	ctx, cancel := context.WithCancel(context.Background())
	go h.handlePermissionAsked(ctx, "c1", "p1", p)
	waitPending(t, h, 2*time.Second)
	cancel()

	call := waitCall(t, agent)
	if call.kind != "permission" || call.reply != oc.PermissionReplyReject {
		t.Errorf("call = %+v, want permission reject on abort", call)
	}
}

// TestHandleQuestionAsked_MapsAnswers verifies multi-question mapping: one
// Choices entry per question, multi-select values split on commas.
func TestHandleQuestionAsked_MapsAnswers(t *testing.T) {
	agent := newRecordingAgent()
	h, _ := newPickerHandlerWithAgent(t, agent)
	q := &oc.QuestionAskedData{
		ID:        "q-1",
		SessionID: "s1",
		Questions: []oc.QuestionInfo{
			{Question: "选模型", Options: []oc.QuestionOption{{Label: "a"}, {Label: "b"}}},
			{Question: "选文件", Multiple: true, Options: []oc.QuestionOption{{Label: "x"}, {Label: "y"}, {Label: "z"}}},
		},
	}

	go h.handleQuestionAsked(context.Background(), "c1", "p1", q)
	waitPending(t, h, 2*time.Second)
	h.Answers.Deliver("q-1", &protocol.AnswerPayload{Choices: []string{"a", "x,y"}})

	call := waitCall(t, agent)
	if call.kind != "question" || call.requestID != "q-1" {
		t.Fatalf("call = %+v, want question q-1", call)
	}
	if len(call.answers) != 2 || call.answers[0][0] != "a" ||
		len(call.answers[1]) != 2 || call.answers[1][0] != "x" || call.answers[1][1] != "y" {
		t.Errorf("answers = %v, want [[a] [x y]]", call.answers)
	}
}

// TestHandleQuestionAsked_CustomInput verifies a custom-typed value answers
// the first question when no option was picked.
func TestHandleQuestionAsked_CustomInput(t *testing.T) {
	agent := newRecordingAgent()
	h, _ := newPickerHandlerWithAgent(t, agent)
	q := &oc.QuestionAskedData{
		ID:        "q-2",
		SessionID: "s1",
		Questions: []oc.QuestionInfo{{Question: "输入分支名", Custom: true}},
	}

	go h.handleQuestionAsked(context.Background(), "c1", "p1", q)
	waitPending(t, h, 2*time.Second)
	h.Answers.Deliver("q-2", &protocol.AnswerPayload{Custom: "feat/x"})

	call := waitCall(t, agent)
	if call.kind != "question" || len(call.answers) != 1 || call.answers[0][0] != "feat/x" {
		t.Errorf("call = %+v, want question answers [[feat/x]]", call)
	}
}

// TestHandleQuestionAsked_IncompleteRejects verifies a partial answer (one of
// two questions unanswered) rejects the request rather than sending
// misaligned answers to the serve server.
func TestHandleQuestionAsked_IncompleteRejects(t *testing.T) {
	agent := newRecordingAgent()
	h, _ := newPickerHandlerWithAgent(t, agent)
	q := &oc.QuestionAskedData{
		ID:        "q-3",
		SessionID: "s1",
		Questions: []oc.QuestionInfo{
			{Question: "问题一", Options: []oc.QuestionOption{{Label: "a"}}},
			{Question: "问题二", Options: []oc.QuestionOption{{Label: "b"}}},
		},
	}

	go h.handleQuestionAsked(context.Background(), "c1", "p1", q)
	waitPending(t, h, 2*time.Second)
	h.Answers.Deliver("q-3", &protocol.AnswerPayload{Choices: []string{"a"}})

	call := waitCall(t, agent)
	if call.kind != "reject" || call.requestID != "q-3" {
		t.Errorf("call = %+v, want reject q-3", call)
	}
}
