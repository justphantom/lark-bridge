package bridgebase

import (
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/router"
)

// TestFormatDuration covers the three time bands.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45秒"},
		{90 * time.Second, "1分30秒"},
		{2 * time.Minute, "2分钟"},
		{70 * time.Minute, "1小时10分"},
		{2 * time.Hour, "2小时"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.d); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

// newTestCore builds a Core over a fresh in-memory router (no persist, no
// rpc) for command/snapshot tests that only exercise router + cancel state.
func newTestCore(t *testing.T) *Core {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	return NewCore(r, nil, CoreConfig{DefaultDirectory: t.TempDir()}, log.Nop())
}

// TestRunningSessions_Empty verifies no in-flight prompts yields no sessions.
func TestRunningSessions_Empty(t *testing.T) {
	c := newTestCore(t)
	if got := c.RunningSessions(); len(got) != 0 {
		t.Errorf("RunningSessions() = %d sessions, want 0", len(got))
	}
}

// TestRunningSessions_DefaultsAndBinding verifies a snapshot reflects the
// cancel entry's start time, the router binding's model/agent, and the
// "(未命名群组)" fallback when no title is set.
func TestRunningSessions_DefaultsAndBinding(t *testing.T) {
	c := newTestCore(t)
	// Bind with empty title, model, and agent; title falls back, model/agent
	// surface from the binding.
	c.Router.Bind("chat-1", "sess-1", "", "", "glm-5.2", "build")
	start := time.Now().Add(-30 * time.Second)
	c.CancelMu.Lock()
	c.CancelByChat["chat-1"] = &PromptCancel{
		Cancel:    func() {},
		StartTime: start,
		ChatID:    "chat-1",
	}
	c.CancelMu.Unlock()

	got := c.RunningSessions()
	if len(got) != 1 {
		t.Fatalf("RunningSessions() = %d sessions, want 1", len(got))
	}
	s := got[0]
	if s.ChatID != "chat-1" {
		t.Errorf("ChatID = %q, want chat-1", s.ChatID)
	}
	if s.Title != "(未命名群组)" {
		t.Errorf("Title = %q, want (未命名群组)", s.Title)
	}
	if s.Model != "glm-5.2" {
		t.Errorf("Model = %q, want glm-5.2", s.Model)
	}
	if s.Agent != "build" {
		t.Errorf("Agent = %q, want build", s.Agent)
	}
	if got, want := FormatDuration(s.Duration), "30秒"; got != want {
		t.Errorf("Duration = %s (%s), want %s", s.Duration, got, want)
	}
}

// TestRunningSessions_OmitsAgentWhenAbsent verifies a backend without an
// agent concept (binding.Agent empty) still snapshots with Agent="" so the
// renderer skips the line.
func TestRunningSessions_OmitsAgentWhenAbsent(t *testing.T) {
	c := newTestCore(t)
	c.Router.Bind("chat-2", "sess-2", "", "群组", "claude-sonnet-4-5", "")
	c.CancelMu.Lock()
	c.CancelByChat["chat-2"] = &PromptCancel{
		Cancel:    func() {},
		StartTime: time.Now(),
		ChatID:    "chat-2",
	}
	c.CancelMu.Unlock()

	got := c.RunningSessions()
	if len(got) != 1 {
		t.Fatalf("RunningSessions() = %d sessions, want 1", len(got))
	}
	if got[0].Agent != "" {
		t.Errorf("Agent = %q, want empty", got[0].Agent)
	}
	if got[0].Title != "群组" {
		t.Errorf("Title = %q, want 群组", got[0].Title)
	}
	if got[0].Model != "claude-sonnet-4-5" {
		t.Errorf("Model = %q, want claude-sonnet-4-5", got[0].Model)
	}
}
