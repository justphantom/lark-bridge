package opencodeservebridge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// idleFakeAgent implements opencodeAPI for testing cmdDeleteIdleSessions. It
// serves per-directory session lists and records which directories were
// queried and which sessions were deleted. The mutex guards the recorded
// slices because deletion runs on a background goroutine after confirmation.
type idleFakeAgent struct {
	mu            sync.Mutex
	sessionsByDir map[string][]oc.SessionInfo
	statuses      map[string]oc.SessionStatus
	dirsCalled    []string
	deleted       []string
}

func (f *idleFakeAgent) Run(context.Context, oc.RunOptions) (<-chan oc.HighEvent, error) {
	return nil, errors.New("not implemented")
}

func (f *idleFakeAgent) ListModels(context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f *idleFakeAgent) ListAgents(context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f *idleFakeAgent) AbortSession(context.Context, string) error {
	return errors.New("not implemented")
}

func (f *idleFakeAgent) ListSessions(_ context.Context, directory string) ([]oc.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirsCalled = append(f.dirsCalled, directory)
	return f.sessionsByDir[directory], nil
}

func (f *idleFakeAgent) SessionStatuses(context.Context) (map[string]oc.SessionStatus, error) {
	return f.statuses, nil
}

func (f *idleFakeAgent) DeleteSessionIfIdle(_ context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, sessionID)
	return nil
}

func (f *idleFakeAgent) ReplyPermission(context.Context, string, string, string) error {
	return errors.New("not implemented")
}

func (f *idleFakeAgent) ReplyQuestion(context.Context, string, *oc.QuestionReply) error {
	return errors.New("not implemented")
}

func (f *idleFakeAgent) RejectQuestion(context.Context, string) error {
	return errors.New("not implemented")
}

func (f *idleFakeAgent) deletedSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

func (f *idleFakeAgent) dirsCalledSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dirsCalled...)
}

// TestCollectIdleSessions_MergesBoundDirectories verifies the candidate
// query lists every bound directory once, merges sessions by ID across
// directories, and skips bound and busy sessions without deleting anything.
func TestCollectIdleSessions_MergesBoundDirectories(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{
			"/a": {{ID: "s1"}, {ID: "s2"}},
			"/b": {{ID: "s2"}, {ID: "s3"}}, // s2 lives in both directories
		},
		statuses: map[string]oc.SessionStatus{
			"s1": {Type: "idle"},
			"s2": {Type: "idle"},
			"s3": {Type: "busy"},
		},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/a", "", "", "") // s1 is bound, must survive
	r.Bind("chat-2", "", "/b", "", "", "")
	r.Bind("chat-3", "", "/a", "", "", "") // duplicate directory, listed once
	r.Bind("chat-4", "", "", "", "", "")   // no directory, ignored

	candidates, skippedBusy, err := h.collectIdleSessions(context.Background())
	if err != nil {
		t.Fatalf("collectIdleSessions: %v", err)
	}

	dirs := agent.dirsCalledSnapshot()
	sort.Strings(dirs)
	if len(dirs) != 2 || dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("dirsCalled = %v, want [/a /b] each once", dirs)
	}
	if len(candidates) != 1 || candidates[0].ID != "s2" {
		t.Errorf("candidates = %v, want only s2 (dedup across directories)", candidates)
	}
	if len(skippedBusy) != 1 || skippedBusy[0] != "s3" {
		t.Errorf("skippedBusy = %v, want [s3]", skippedBusy)
	}
	if got := agent.deletedSnapshot(); len(got) != 0 {
		t.Errorf("deleted = %v, want none (collect is a pure query)", got)
	}
}

// TestCmdDeleteIdleSessions_Confirm_DeletesSessions drives the async confirm
// path: with candidates the command returns Handled and pops a card; picking
// "确认清理" deletes exactly the candidates.
func TestCmdDeleteIdleSessions_Confirm_DeletesSessions(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{
			"/a": {{ID: "s1", Title: "绑定会话"}, {ID: "s2", Title: "空闲会话"}},
		},
		statuses: map[string]oc.SessionStatus{
			"s1": {Type: "idle"},
			"s2": {Type: "idle"},
		},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/a", "", "", "")

	res, err := h.cmdDeleteIdleSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdDeleteIdleSessions: %v", err)
	}
	if !res.Handled {
		t.Error("confirm flow should return Handled=true immediately")
	}

	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"确认清理"}, MessageID: "msg-1"})

	waitDeleted(t, agent, []string{"s2"}, 2*time.Second)
}

// TestCmdDeleteIdleSessions_Cancel_KeepsSessions verifies picking "取消"
// deletes nothing.
func TestCmdDeleteIdleSessions_Cancel_KeepsSessions(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{
			"/a": {{ID: "s2", Title: "空闲会话"}},
		},
		statuses: map[string]oc.SessionStatus{"s2": {Type: "idle"}},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "", "/a", "", "", "")

	if _, err := h.cmdDeleteIdleSessions(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdDeleteIdleSessions: %v", err)
	}

	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"取消"}, MessageID: "msg-1"})
	waitAnswerConsumed(t, h, 2*time.Second)
	// Deliver frees the slot synchronously; give the goroutine a beat to act
	// so a wrongful deletion would show up before we assert.
	time.Sleep(50 * time.Millisecond)

	if got := agent.deletedSnapshot(); len(got) != 0 {
		t.Errorf("deleted = %v, want none after 取消", got)
	}
}

// TestCmdDeleteIdleSessions_AnswerError_KeepsSessions verifies an answer
// carrying neither choice nor custom text (the AskAndWait error path, same
// code path as a timeout) deletes nothing.
func TestCmdDeleteIdleSessions_AnswerError_KeepsSessions(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{
			"/a": {{ID: "s2", Title: "空闲会话"}},
		},
		statuses: map[string]oc.SessionStatus{"s2": {Type: "idle"}},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "", "/a", "", "", "")

	if _, err := h.cmdDeleteIdleSessions(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdDeleteIdleSessions: %v", err)
	}

	reqID := waitPending(t, h, time.Second)
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{})
	waitAnswerConsumed(t, h, 2*time.Second)
	time.Sleep(50 * time.Millisecond) // let the goroutine act before asserting

	if got := agent.deletedSnapshot(); len(got) != 0 {
		t.Errorf("deleted = %v, want none after answer error", got)
	}
}

// TestCmdDeleteIdleSessions_NoCandidates_ReturnsText verifies that with no
// candidates the command replies with plain text and never pops a card.
func TestCmdDeleteIdleSessions_NoCandidates_ReturnsText(t *testing.T) {
	agent := &idleFakeAgent{
		sessionsByDir: map[string][]oc.SessionInfo{
			"/a": {{ID: "s1"}},
		},
		statuses: map[string]oc.SessionStatus{"s1": {Type: "idle"}},
	}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "s1", "/a", "", "", "") // s1 bound → no candidates

	res, err := h.cmdDeleteIdleSessions(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdDeleteIdleSessions: %v", err)
	}
	if res.Handled {
		t.Error("no-candidate path should not be Handled (no async card flow)")
	}
	if !contains(res.Body, "没有可清理") {
		t.Errorf("Body = %q, want '没有可清理'", res.Body)
	}
	// Give any (unexpectedly spawned) goroutine a moment, then assert no
	// pending answer slot and no deletion.
	time.Sleep(20 * time.Millisecond)
	if ids := h.Answers.PendingIDs(); len(ids) != 0 {
		t.Errorf("PendingIDs = %v, want none (no card should be popped)", ids)
	}
	if got := agent.deletedSnapshot(); len(got) != 0 {
		t.Errorf("deleted = %v, want none", got)
	}
}

// TestCleanConfirmLabel verifies the label carries the total count plus the
// first 10 sessions (title + ID), truncating longer lists with a "…等 N 个"
// footer.
func TestCleanConfirmLabel(t *testing.T) {
	t.Run("within limit lists every session", func(t *testing.T) {
		label := cleanConfirmLabel([]oc.SessionInfo{
			{ID: "s1", Title: "第一个"},
			{ID: "s2"}, // empty title falls back to (未命名会话)
		})
		for _, want := range []string{"2 个", "第一个", "`s1`", "(未命名会话)", "`s2`"} {
			if !contains(label, want) {
				t.Errorf("label = %q, missing %q", label, want)
			}
		}
		if contains(label, "…等") {
			t.Errorf("label = %q, unexpected truncation footer", label)
		}
	})

	t.Run("over limit truncates to ten with footer", func(t *testing.T) {
		candidates := make([]oc.SessionInfo, 12)
		for i := range candidates {
			candidates[i] = oc.SessionInfo{ID: fmt.Sprintf("s%02d", i), Title: fmt.Sprintf("会话%d", i)}
		}
		label := cleanConfirmLabel(candidates)
		if !contains(label, "12 个") || !contains(label, "…等 12 个") {
			t.Errorf("label = %q, want total count + '…等 12 个'", label)
		}
		if !contains(label, "`s09`") {
			t.Errorf("label = %q, want the 10th session (s09) shown", label)
		}
		if contains(label, "`s10`") {
			t.Errorf("label = %q, want the 11th session (s10) truncated", label)
		}
	})
}

// waitDeleted polls the fake agent until its recorded deletions equal want
// or the timeout elapses.
func waitDeleted(t *testing.T, agent *idleFakeAgent, want []string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := agent.deletedSnapshot()
		if len(got) == len(want) {
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("deleted = %v, want %v", got, want)
				}
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("deleted = %v, want %v", agent.deletedSnapshot(), want)
}

// waitAnswerConsumed polls until no answer slot is pending, i.e. the
// background goroutine received the delivered answer and returned.
func waitAnswerConsumed(t *testing.T, h *Handler, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(h.Answers.PendingIDs()) == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("answer slot still pending; goroutine did not consume the answer")
}
