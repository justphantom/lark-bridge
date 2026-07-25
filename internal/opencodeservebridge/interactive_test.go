package opencodeservebridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// replyCall records one serve reply invocation: kind is "permission" /
// "question" / "reject", requestID the serve-side request id.
type replyCall struct {
	kind      string
	requestID string
	directory string
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

func (a *recordingAgent) ReplyPermission(_ context.Context, requestID, directory, reply, _ string) error {
	a.mu.Lock()
	a.calls = append(a.calls, replyCall{kind: "permission", requestID: requestID, reply: reply, directory: directory})
	a.mu.Unlock()
	a.notify()
	return nil
}

func (a *recordingAgent) ReplyQuestion(_ context.Context, requestID, directory string, r *oc.QuestionReply) error {
	a.mu.Lock()
	a.calls = append(a.calls, replyCall{kind: "question", requestID: requestID, answers: r.Answers, directory: directory})
	a.mu.Unlock()
	a.notify()
	return nil
}

func (a *recordingAgent) RejectQuestion(_ context.Context, requestID, directory string) error {
	a.mu.Lock()
	a.calls = append(a.calls, replyCall{kind: "reject", requestID: requestID, directory: directory})
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
		{"始终允许（全局）", oc.PermissionReplyAlways},
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

// TestHandleQuestionAsked_RepliesWithDirectory pins the directory fix: the
// reply MUST carry the chat binding's directory, or opencode serve (which
// isolates pending requests by directory) returns 404 "unknown request".
func TestHandleQuestionAsked_RepliesWithDirectory(t *testing.T) {
	agent := newRecordingAgent()
	h, r := newPickerHandlerWithAgent(t, agent)
	r.Bind("c1", "s1", "/repo", "", "", "")
	q := &oc.QuestionAskedData{
		ID:        "q-dir",
		SessionID: "s1",
		Questions: []oc.QuestionInfo{
			{Question: "选", Options: []oc.QuestionOption{{Label: "a"}}},
		},
	}

	go h.handleQuestionAsked(context.Background(), "c1", "p1", q)
	waitPending(t, h, 2*time.Second)
	h.Answers.Deliver("q-dir", &protocol.AnswerPayload{Choices: []string{"a"}})

	call := waitCall(t, agent)
	if call.directory != "/repo" {
		t.Errorf("directory = %q, want /repo (serve isolates requests by directory)", call.directory)
	}
}

// TestHandleQuestionAsked_MultiQuestionCustomRejected pins P2-15: when the
// user submits a Custom value alongside multiple questions, the bridge
// rejects server-side and surfaces a notice instead of silently mapping
// the custom text to answers[0] and dropping the rest. The notice wording
// is asserted by the parallel capture test in commands_interactive_test.go;
// here we only pin the reject path.
func TestHandleQuestionAsked_MultiQuestionCustomRejected(t *testing.T) {
	agent := newRecordingAgent()
	h, _ := newPickerHandlerWithAgent(t, agent)
	q := &oc.QuestionAskedData{
		ID:        "q-multi",
		SessionID: "s1",
		Questions: []oc.QuestionInfo{
			{Question: "选模型", Options: []oc.QuestionOption{{Label: "a"}}},
			{Question: "选文件", Options: []oc.QuestionOption{{Label: "x"}}},
		},
	}

	go h.handleQuestionAsked(context.Background(), "c1", "p1", q)
	waitPending(t, h, 2*time.Second)
	// User typed a custom value instead of picking options for both
	// questions. Single-question + Custom still maps (separate test); the
	// multi-question path must reject to avoid silent partial answers.
	h.Answers.Deliver("q-multi", &protocol.AnswerPayload{Custom: "do the thing"})

	call := waitCall(t, agent)
	if call.kind != "reject" {
		t.Errorf("call = %+v, want kind=reject for multi-question + custom", call)
	}
	if call.requestID != "q-multi" {
		t.Errorf("requestID = %q, want q-multi", call.requestID)
	}
}

// TestHandlePermissionAsked_RepliesWithDirectory is the permission analogue
// of the question directory test above.
func TestHandlePermissionAsked_RepliesWithDirectory(t *testing.T) {
	agent := newRecordingAgent()
	h, r := newPickerHandlerWithAgent(t, agent)
	r.Bind("c1", "s1", "/repo", "", "", "")
	p := &oc.PermissionAskedData{ID: "perm-dir", SessionID: "s1", Permission: "bash"}

	go h.handlePermissionAsked(context.Background(), "c1", "p1", p)
	waitPending(t, h, 2*time.Second)
	h.Answers.Deliver("perm-dir", &protocol.AnswerPayload{Choices: []string{"允许一次"}})

	call := waitCall(t, agent)
	if call.directory != "/repo" {
		t.Errorf("directory = %q, want /repo", call.directory)
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

// TestHandleQuestionAsked_EmitsAnswerBanner verifies that after a successful
// ReplyQuestion the backend emits a TypeProgress gate banner (answered) on the
// progress card, so the user sees the answer in the ongoing turn without
// scrolling back to the standalone question card. The prior TypeText echo was
// dead (the dispatcher drops TypeText); the gate banner replaces it.
func TestHandleQuestionAsked_EmitsAnswerBanner(t *testing.T) {
	agent := newRecordingAgent()
	client, reg, rpcCleanup := connectTestRPC(t)
	defer rpcCleanup()

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, agent, client, HandlerConfig{StateDir: t.TempDir()}, log.Nop())

	q := &oc.QuestionAskedData{
		ID:        "q-fb",
		SessionID: "s1",
		Questions: []oc.QuestionInfo{
			{Question: "选模型", Options: []oc.QuestionOption{{Label: "gpt-4"}}},
		},
	}

	go h.handleQuestionAsked(context.Background(), "c1", "p1", q)
	waitPending(t, h, 2*time.Second)
	h.Answers.Deliver("q-fb", &protocol.AnswerPayload{Choices: []string{"gpt-4"}})

	call := waitCall(t, agent)
	if call.kind != "question" || call.requestID != "q-fb" {
		t.Fatalf("call = %+v, want question q-fb", call)
	}

	// Drain gate banners; expect waiting (pre-pick) then answered (post-pick).
	// EmitAsync is unordered, so drain the full window for both rather than
	// stopping at the first answered.
	deadline := time.Now().Add(3 * time.Second)
	var waiting, answered *protocol.GateInfo
	for time.Now().Before(deadline) && (waiting == nil || answered == nil) {
		select {
		case rc := <-reg.Controls():
			c := rc.Control
			if c.Type == protocol.TypeProgress && c.Progress != nil && c.Progress.Gate != nil {
				switch c.Progress.Gate.State {
				case "waiting":
					waiting = c.Progress.Gate
				case "answered":
					answered = c.Progress.Gate
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if waiting == nil {
		t.Error("expected a TypeProgress gate banner with state=waiting before answer, got none")
	} else if !strings.Contains(waiting.Summary, "选模型") {
		t.Errorf("waiting summary = %q, want contains '选模型'", waiting.Summary)
	}
	if answered == nil {
		t.Fatal("expected a TypeProgress gate banner with state=answered, got none")
	}
	if !strings.Contains(answered.Summary, "gpt-4") {
		t.Errorf("answered summary = %q, want contains 'gpt-4'", answered.Summary)
	}
}

// TestHandlePermissionAsked_EmitsAnswerBanner mirrors the question-path banner
// test: after a successful ReplyPermission the backend emits a TypeProgress
// gate banner (answered) onto the progress card, plus a waiting banner that
// preceded the pick. The prior TypeText echo was dead (dispatcher drops it).
func TestHandlePermissionAsked_EmitsAnswerBanner(t *testing.T) {
	agent := newRecordingAgent()
	client, reg, rpcCleanup := connectTestRPC(t)
	defer rpcCleanup()

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, agent, client, HandlerConfig{StateDir: t.TempDir()}, log.Nop())

	p := &oc.PermissionAskedData{
		ID:         "perm-fb",
		SessionID:  "s1",
		Permission: "bash",
		Patterns:   []string{"make test"},
	}

	go h.handlePermissionAsked(context.Background(), "c1", "p1", p)
	waitPending(t, h, 2*time.Second)
	h.Answers.Deliver("perm-fb", &protocol.AnswerPayload{Choices: []string{"允许一次"}})

	call := waitCall(t, agent)
	if call.kind != "permission" || call.requestID != "perm-fb" {
		t.Fatalf("call = %+v, want permission perm-fb", call)
	}

	// Drain gate banners; expect waiting (pre-pick) then answered (post-pick).
	// EmitAsync is unordered, so drain the full window for both.
	deadline := time.Now().Add(3 * time.Second)
	var waiting, answered *protocol.GateInfo
	for time.Now().Before(deadline) && (waiting == nil || answered == nil) {
		select {
		case rc := <-reg.Controls():
			c := rc.Control
			if c.Type == protocol.TypeProgress && c.Progress != nil && c.Progress.Gate != nil {
				switch c.Progress.Gate.State {
				case "waiting":
					waiting = c.Progress.Gate
				case "answered":
					answered = c.Progress.Gate
				}
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if waiting == nil {
		t.Error("expected a TypeProgress gate banner with state=waiting before answer, got none")
	} else if !strings.Contains(waiting.Summary, "bash") {
		t.Errorf("waiting summary = %q, want contains 'bash'", waiting.Summary)
	}
	if answered == nil {
		t.Fatal("expected a TypeProgress gate banner with state=answered, got none")
	}
	if !strings.Contains(answered.Summary, "允许一次") {
		t.Errorf("answered summary = %q, want contains '允许一次'", answered.Summary)
	}
}
