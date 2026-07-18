package miniagent

import (
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniclient"
)

// newCLIHandler builds a Handler wired only enough to call emitCLIEvent:
// the rpc captures Controls, no LLM/client/history needed.
func newCLIHandler(t *testing.T) (*Handler, *captureSender) {
	t.Helper()
	sender := &captureSender{}
	h := New(nil, LoopConfig{}, sender, log.Nop(), nil, nil, "", nil, "default", nil)
	return h, sender
}

// TestEmitCLIEvent_Incomplete_PropagatesAndFillsText verifies that when the
// CLI signals it hit the iteration cap (result.incomplete=true with empty
// text), the bridge:
//  1. surfaces Incomplete=true on the emitted ResultPayload, and
//  2. fills Text with a self-explaining placeholder instead of sending the
//     user a blank card.
//
// Regression guard: before Incomplete was parsed, the bridge reported an
// empty reply as if it were a normal success.
func TestEmitCLIEvent_Incomplete_PropagatesAndFillsText(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:       miniclient.KindResult,
		Model:      "kimi",
		Steps:      20,
		Incomplete: true,
		IsTerminal: true,
	}
	h.emitCLIEvent("chat-1", "prompt-1", ev, time.Now())

	got := sender.Controls()
	if len(got) != 1 {
		t.Fatalf("emits=%d, want 1", len(got))
	}
	r := got[0].Result
	if r == nil {
		t.Fatal("control is not a Result")
	}
	if !r.Incomplete {
		t.Error("ResultPayload.Incomplete = false, want true")
	}
	if r.Text == "" {
		t.Error("Text should be filled with a placeholder when incomplete and empty")
	}
	if r.Steps != 20 {
		t.Errorf("Steps = %d, want 20", r.Steps)
	}
}

// TestEmitCLIEvent_NormalResultUnchanged verifies a normal result (no
// incomplete) still reaches the frontend untouched: Incomplete must be
// false and Text must be the LLM's reply, not the placeholder.
func TestEmitCLIEvent_NormalResultUnchanged(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:       miniclient.KindResult,
		Text:       "测试全部通过。",
		Model:      "kimi",
		InputTokens: 320,
		OutputTokens: 48,
		Steps:      3,
		IsTerminal: true,
	}
	h.emitCLIEvent("chat-1", "prompt-1", ev, time.Now())

	got := sender.Controls()
	if len(got) != 1 || got[0].Result == nil {
		t.Fatalf("want one Result, got %+v", got)
	}
	r := got[0].Result
	if r.Incomplete {
		t.Error("Incomplete should be false for a normal result")
	}
	if r.Text != "测试全部通过。" {
		t.Errorf("Text = %q, want original reply", r.Text)
	}
}

// TestEmitCLIEvent_IncompleteWithTextKeepsText verifies that even when
// incomplete=true, a non-empty Text from the CLI is preserved — the
// placeholder fills only the empty case.
func TestEmitCLIEvent_IncompleteWithTextKeepsText(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:       miniclient.KindResult,
		Text:       "partial summary...",
		Steps:      20,
		Incomplete: true,
		IsTerminal: true,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	got := sender.Controls()
	if len(got) != 1 || got[0].Result == nil {
		t.Fatalf("want one Result, got %+v", got)
	}
	r := got[0].Result
	if !r.Incomplete {
		t.Error("Incomplete should propagate even when Text is present")
	}
	if r.Text != "partial summary..." {
		t.Errorf("Text = %q, want CLI's original text preserved", r.Text)
	}
}
