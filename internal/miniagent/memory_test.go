package miniagent

import (
	"strings"
	"testing"

	"github.com/hu/lark-bridge/internal/log"
)

// newTestHistory builds a History rooted at a temp dir.
func newTestHistory(t *testing.T) *History {
	t.Helper()
	return NewHistory(t.TempDir(), log.Nop())
}

// TestHistory_LoadMissing verifies a fresh chat has no history.
func TestHistory_LoadMissing(t *testing.T) {
	h := newTestHistory(t)
	if got := h.Load("chat-new"); got != nil {
		t.Errorf("Load new chat = %v, want nil", got)
	}
}

// TestHistory_NilSafe verifies a nil History (memory disabled) is safe.
func TestHistory_NilSafe(t *testing.T) {
	var h *History
	if got := h.Load("x"); got != nil {
		t.Errorf("nil Load = %v, want nil", got)
	}
	h.Append("x", []Message{{Role: "user", Content: "hi"}}) // must not panic
}

// TestHistory_AppendThenLoad verifies a round-trip preserves messages.
func TestHistory_AppendThenLoad(t *testing.T) {
	h := newTestHistory(t)
	msgs := []Message{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}
	h.Append("chat-1", msgs)
	got := h.Load("chat-1")
	if len(got) != 2 {
		t.Fatalf("Load = %d msgs, want 2", len(got))
	}
	if got[0].Content != "hello" || got[1].Content != "hi there" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

// TestHistory_AppendAccumulates verifies multiple Appends add up.
func TestHistory_AppendAccumulates(t *testing.T) {
	h := newTestHistory(t)
	h.Append("c", []Message{{Role: "user", Content: "turn1"}})
	h.Append("c", []Message{{Role: "assistant", Content: "reply1"}})
	h.Append("c", []Message{{Role: "user", Content: "turn2"}})
	got := h.Load("c")
	if len(got) != 3 {
		t.Fatalf("Load = %d msgs, want 3", len(got))
	}
	if got[2].Content != "turn2" {
		t.Errorf("last msg = %q, want turn2", got[2].Content)
	}
}

// TestHistory_DistinctChats verifies each chatID has its own file.
func TestHistory_DistinctChats(t *testing.T) {
	h := newTestHistory(t)
	h.Append("chat-a", []Message{{Role: "user", Content: "A"}})
	h.Append("chat-b", []Message{{Role: "user", Content: "B"}})
	if got := h.Load("chat-a"); len(got) != 1 || got[0].Content != "A" {
		t.Errorf("chat-a = %+v", got)
	}
	if got := h.Load("chat-b"); len(got) != 1 || got[0].Content != "B" {
		t.Errorf("chat-b = %+v", got)
	}
}

// TestHistory_TrimKeepsToolPairing verifies that when trimming drops a turn
// containing an assistant tool_call, the matching tool-role result goes with
// it — OpenAI rejects an orphan tool message or an orphan tool_call.
func TestHistory_TrimKeepsToolPairing(t *testing.T) {
	h := newTestHistory(t)
	// Build a history well over the token cap so trim must fire, with a
	// tool_call/tool_result pair in the oldest turn.
	big := strings.Repeat("x ", maxHistoryTokens*3) // ~6x the cap per message
	msgs := []Message{
		{Role: "user", Content: big},                                   // turn 1 (old, huge)
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"x"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: big},
		{Role: "assistant", Content: big},
		{Role: "user", Content: "recent question"},                     // turn 2 (keep)
		{Role: "assistant", Content: "recent answer"},                  // keep
	}
	got := h.trim(msgs)
	// The first turn (4 messages) must be entirely gone; only the recent
	// user+assistant pair survives.
	if len(got) != 2 {
		t.Fatalf("trim = %d msgs, want 2 (recent turn only): %+v", len(got), got)
	}
	// No orphan tool message and no orphan tool_call may remain.
	for _, m := range got {
		if m.Role == "tool" {
			t.Errorf("trim left orphan tool message: %+v", m)
		}
		if len(m.ToolCalls) > 0 {
			t.Errorf("trim left orphan tool_call: %+v", m)
		}
	}
}

// TestHistory_TrimKeepsSingleTurn verifies trim never drops the only turn,
// even if it exceeds the cap (better to over-send than to leave the LLM
// with no context for the current user message).
func TestHistory_TrimKeepsSingleTurn(t *testing.T) {
	h := newTestHistory(t)
	big := strings.Repeat("x ", maxHistoryTokens*3)
	msgs := []Message{
		{Role: "user", Content: big},
		{Role: "assistant", Content: big},
	}
	got := h.trim(msgs)
	if len(got) != 2 {
		t.Errorf("trim single turn = %d msgs, want 2 (unchanged)", len(got))
	}
}

// TestHistory_TrimUnderCapNoop verifies under-cap history is unchanged.
func TestHistory_TrimUnderCapNoop(t *testing.T) {
	h := newTestHistory(t)
	msgs := []Message{
		{Role: "user", Content: "short"},
		{Role: "assistant", Content: "reply"},
	}
	got := h.trim(msgs)
	if len(got) != 2 {
		t.Errorf("trim under cap = %d msgs, want 2", len(got))
	}
}

// TestHistory_SessionPathSanitized verifies a chatID with path separators
// cannot escape the history dir. SanitizeName keeps '.' (so ".." survives as
// a literal filename fragment) but collapses '/', which is what prevents the
// path traversal — the session path is one flat filename under history/, not
// a nested path. sessionPath is the successor to the removed pathFor.
func TestHistory_SessionPathSanitized(t *testing.T) {
	h := newTestHistory(t)
	p := h.sessionPath("oc_../../etc/passwd", "20260101-120000")
	if !strings.HasSuffix(p, ".jsonl") {
		t.Errorf("path missing .jsonl suffix: %s", p)
	}
	// The sanitized basename must be a single path component (no '/'),
	// so the file lands directly under history/.
	base := p[strings.LastIndex(p, "/")+1:]
	if base == "" {
		t.Fatalf("empty basename: %s", p)
	}
	if strings.Contains(base, "/") {
		t.Errorf("basename contains '/', path traversal possible: %s", base)
	}
}
