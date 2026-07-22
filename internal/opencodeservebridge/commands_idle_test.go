package opencodeservebridge

import (
	"context"
	"errors"
	"sort"
	"testing"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// idleFakeAgent implements opencodeAPI for testing cmdDeleteIdleSessions. It
// serves per-directory session lists and records which directories were
// queried and which sessions were deleted.
type idleFakeAgent struct {
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
	f.dirsCalled = append(f.dirsCalled, directory)
	return f.sessionsByDir[directory], nil
}

func (f *idleFakeAgent) SessionStatuses(context.Context) (map[string]oc.SessionStatus, error) {
	return f.statuses, nil
}

func (f *idleFakeAgent) DeleteSessionIfIdle(_ context.Context, sessionID string) error {
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

// TestCmdDeleteIdleSessions_MergesBoundDirectories verifies /session-clean
// lists every bound directory once, merges sessions by ID across directories,
// and still skips bound and busy sessions.
func TestCmdDeleteIdleSessions_MergesBoundDirectories(t *testing.T) {
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

	res, err := h.cmdDeleteIdleSessions(context.Background(), "chat-2", nil)
	if err != nil {
		t.Fatalf("cmdDeleteIdleSessions: %v", err)
	}

	dirs := append([]string(nil), agent.dirsCalled...)
	sort.Strings(dirs)
	if len(dirs) != 2 || dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("dirsCalled = %v, want [/a /b] each once", agent.dirsCalled)
	}
	if len(agent.deleted) != 1 || agent.deleted[0] != "s2" {
		t.Errorf("deleted = %v, want [s2] exactly once (dedup across directories)", agent.deleted)
	}
	if !contains(res.Body, "已删除 1 个") || !contains(res.Body, "跳过 1 个") {
		t.Errorf("Body = %q, want '已删除 1 个' + '跳过 1 个' (busy s3)", res.Body)
	}
}
