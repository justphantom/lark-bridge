package opencodeservebridge

import (
	"context"
	"errors"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// fakeListAgent implements opencodeAPI for testing cmdListSessions.
type fakeListAgent struct {
	sessions []oc.SessionInfo
	err      error
}

func (f fakeListAgent) Run(ctx context.Context, opts oc.RunOptions) (<-chan oc.HighEvent, error) {
	return nil, errors.New("not implemented")
}

func (f fakeListAgent) ListModels(ctx context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f fakeListAgent) ListAgents(ctx context.Context) ([]string, error) {
	return nil, errors.New("not implemented")
}

func (f fakeListAgent) AbortSession(ctx context.Context, sessionID string) error {
	return errors.New("not implemented")
}

func (f fakeListAgent) ListSessions(ctx context.Context) ([]oc.SessionInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sessions, nil
}

func (f fakeListAgent) SessionStatuses(ctx context.Context) (map[string]oc.SessionStatus, error) {
	return nil, errors.New("not implemented")
}

func (f fakeListAgent) DeleteSessionIfIdle(ctx context.Context, sessionID string) error {
	return errors.New("not implemented")
}

// TestCmdListSessions_Empty verifies the command reports empty state.
func TestCmdListSessions_Empty(t *testing.T) {
	h := newHandlerWithAgent(t, fakeListAgent{sessions: []oc.SessionInfo{}})

	res, err := h.cmdListSessions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if res.Body != "当前没有任何会话。" {
		t.Errorf("Body = %q, want '当前没有任何会话。'", res.Body)
	}
}

// TestCmdListSessions_Error verifies the command reports list errors.
func TestCmdListSessions_Error(t *testing.T) {
	h := newHandlerWithAgent(t, fakeListAgent{err: errors.New("连接失败")})

	res, err := h.cmdListSessions(context.Background(), "", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, errors.New("连接失败")) && err.Error() != "连接失败" {
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
	h := newHandlerWithAgent(t, fakeListAgent{sessions: sessions})

	res, err := h.cmdListSessions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
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
	h := newHandlerWithAgent(t, fakeListAgent{sessions: sessions})

	res, err := h.cmdListSessions(context.Background(), "", nil)
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

// TestFormatSessions_VerifyAllFields covers various session field combinations.
func TestFormatSessions_VerifyAllFields(t *testing.T) {
	now := time.Now().UnixMilli()
	sessions := []oc.SessionInfo{
		{
			ID:     "full",
			Title:  "完整字段",
			Agent:  "reviewer",
			Model:  &oc.ModelRef{ID: "claude-3-5-sonnet", ProviderID: "anthropic"},
			Cost:   0.5678,
			Tokens: oc.SessionTokens{Input: 100, Output: 200, Reasoning: 5000, Cache: oc.SessionCache{Read: 1000, Write: 500}},
			Time:   oc.SessionTime{Updated: now},
		},
		{
			ID:     "min",
			Title:  "",
			Agent:  "",
			Model:  nil,
			Cost:   0,
			Tokens: oc.SessionTokens{},
			Time:   oc.SessionTime{Updated: now - 86400000}, // 1 day
		},
		{
			ID:     "zero-time",
			Title:  "零时间戳",
			Agent:  "",
			Model:  nil,
			Cost:   0,
			Tokens: oc.SessionTokens{Input: 10},
			Time:   oc.SessionTime{Updated: 0},
		},
	}
	h := newHandlerWithAgent(t, fakeListAgent{sessions: sessions})

	res, err := h.cmdListSessions(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}

	body := res.Body
	// Full session shows all fields
	for _, want := range []string{"完整字段", "reviewer", "anthropic/claude-3-5-sonnet", "$0.5678", "5000", "缓存", "刚刚"} {
		if !contains(body, want) {
			t.Errorf("Body missing %q\ngot:\n%s", want, body)
		}
	}
	// Min session defaults
	for _, want := range []string{"(未命名会话)", "1天前"} {
		if !contains(body, want) {
			t.Errorf("Body missing %q\ngot:\n%s", want, body)
		}
	}
	// Zero time shows "(未知)"
	if !contains(body, "(未知)") {
		t.Errorf("Body missing '(未知)' for zero timestamp\ngot:\n%s", body)
	}
}

// TestModelString covers ModelRef formatting.
func TestModelString(t *testing.T) {
	tests := []struct {
		name string
		m    *oc.ModelRef
		want string
	}{
		{"full", &oc.ModelRef{ID: "gpt-4", ProviderID: "openai"}, "openai/gpt-4"},
		{"id only", &oc.ModelRef{ID: "model-xyz", ProviderID: ""}, "model-xyz"},
		{"nil", nil, "(默认)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := modelString(tt.m); got != tt.want {
				t.Errorf("modelString() = %q, want %q", got, tt.want)
			}
		})
	}
}

// newHandlerWithAgent creates a Handler with the given fake agent for testing.
func newHandlerWithAgent(t *testing.T, agent opencodeAPI) *Handler {
	t.Helper()
	h := &Handler{agent: agent}
	return h
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
