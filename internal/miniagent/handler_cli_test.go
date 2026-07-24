package miniagent

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/router"
)

// newCLIHandler builds a Handler wired only enough to call emitCLIEvent:
// the rpc captures Controls, no client/LLM needed.
func newCLIHandler(t *testing.T) (*Handler, *captureSender) {
	t.Helper()
	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, "", "test-model", nil)
	return h, sender
}

// TestEmitCLIEvent_NormalResult verifies a result event reaches the frontend
// with text/model/tokens/steps propagated unchanged.
func TestEmitCLIEvent_NormalResult(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:         miniclient.KindResult,
		Text:         "测试全部通过。",
		Model:        "kimi",
		InputTokens:  320,
		OutputTokens: 48,
		Steps:        3,
		IsTerminal:   true,
	}
	h.emitCLIEvent("chat-1", "prompt-1", ev, time.Now())

	got := sender.Controls()
	if len(got) != 1 || got[0].Result == nil {
		t.Fatalf("want one Result, got %+v", got)
	}
	r := got[0].Result
	if r.Text != "测试全部通过。" {
		t.Errorf("Text = %q, want original reply", r.Text)
	}
	if r.Model != "kimi" {
		t.Errorf("Model = %q, want kimi", r.Model)
	}
	if r.Steps != 3 {
		t.Errorf("Steps = %d, want 3", r.Steps)
	}
	if r.Tokens != 368 {
		t.Errorf("Tokens = %d, want 368 (in+out)", r.Tokens)
	}
}

// TestEmitCLIEvent_EmptyResultText verifies an empty-text result still
// emits a Result control (no placeholder fill after the Incomplete field
// was removed in the stateless migration).
func TestEmitCLIEvent_EmptyResultText(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:       miniclient.KindResult,
		Text:       "",
		Model:      "kimi",
		Steps:      1,
		IsTerminal: true,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	got := sender.Controls()
	if len(got) != 1 || got[0].Result == nil {
		t.Fatalf("want one Result, got %+v", got)
	}
	if got[0].Result.Text != "" {
		t.Errorf("Text = %q, want empty passed through", got[0].Result.Text)
	}
}

// TestEmitCLIEvent_ToolUse verifies a tool_use event maps to a ToolUse
// payload with name/input propagated.
func TestEmitCLIEvent_ToolUse(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:  miniclient.KindToolUse,
		Name:  "read_file",
		Input: `{"path":"x"}`,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	got := sender.Controls()
	if len(got) != 1 || got[0].ToolUse == nil {
		t.Fatalf("want one ToolUse, got %+v", got)
	}
	if got[0].ToolUse.Name != "read_file" {
		t.Errorf("Name = %q, want read_file", got[0].ToolUse.Name)
	}
}

// TestEmitCLIEvent_Error verifies an error event maps to an Error payload.
func TestEmitCLIEvent_Error(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:       miniclient.KindError,
		Message:    "boom",
		IsTerminal: true,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	got := sender.Controls()
	if len(got) != 1 || got[0].Error == nil {
		t.Fatalf("want one Error, got %+v", got)
	}
	if got[0].Error.Message != "boom" {
		t.Errorf("Message = %q, want boom", got[0].Error.Message)
	}
}

// TestActiveTurnConfig_DefaultsNoBinding verifies that without a router the
// global defaults (cfgModel, workspaceRoot) are returned.
func TestActiveTurnConfig_DefaultsNoBinding(t *testing.T) {
	h, _ := newCLIHandler(t)
	model, dir := h.activeTurnConfig("c1")
	if model != "test-model" {
		t.Errorf("model = %q, want test-model", model)
	}
	if dir != "" {
		t.Errorf("dir = %q, want empty (no workspaceRoot configured)", dir)
	}
}

// TestActiveTurnConfig_BoundOverridesDefault verifies that when a chat has a
// router binding with ModelSpec/Directory set, activeTurnConfig returns those
// bound values instead of the global defaults. This is the central code path
// of the miniagent-back stateless migration: the bridge keeps no per-chat
// session — the router binding is the only per-chat state, spliced into CLI
// flags at fork time via activeTurnConfig.
func TestActiveTurnConfig_BoundOverridesDefault(t *testing.T) {
	r, err := router.New(filepath.Join(t.TempDir(), "r.json"), log.Nop())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	defer r.Close()

	// Set* are no-ops until a binding exists (mutate drops silently on miss),
	// so mirror ensureBinding: Bind first, then mutate the fields.
	r.Bind("c1", "", "", "", "", "")
	r.SetModelSpec("c1", "kimi")
	r.SetDirectory("c1", "/proj")

	h := New(&captureSender{}, log.Nop(), r, "/global-root", "test-model", nil)

	if model, _ := h.activeTurnConfig("c1"); model != "kimi" {
		t.Errorf("bound model = %q, want kimi", model)
	}
	if _, dir := h.activeTurnConfig("c1"); dir != "/proj" {
		t.Errorf("bound dir = %q, want /proj", dir)
	}

	// A chat without a binding still gets the global defaults — proves the
	// override is per-chat, not process-wide.
	if model, dir := h.activeTurnConfig("no-such-chat"); model != "test-model" || dir != "/global-root" {
		t.Errorf("unbound = (%q, %q), want (test-model, /global-root)", model, dir)
	}
}
