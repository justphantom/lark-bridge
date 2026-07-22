package opencodeservebridge

import (
	"context"
	"testing"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

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
	agent := &fakeListAgent{sessions: sessions}
	h, r := newHandlerWithAgent(t, agent)
	r.Bind("chat-1", "", "/proj", "", "", "")

	res, err := h.cmdListSessions(context.Background(), "chat-1", nil)
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
