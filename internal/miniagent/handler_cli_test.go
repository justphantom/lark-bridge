package miniagent

import (
	"path/filepath"
	"strings"
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
	h := New(sender, log.Nop(), nil, "", "test-model", nil, 0, "", false)
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

// TestEmitCLIEvent_EmptyResultText verifies a result with empty text AND empty
// finish stays empty: only finish=max_iterations fills a placeholder (see
// TestEmitCLIEvent_MaxIterations). A pre-v1.1.0 CLI (no finish field) hitting
// an empty reply still surfaces an empty card.
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

// TestEmitCLIEvent_MaxIterations verifies a result with finish=max_iterations
// flips Incomplete and fills the empty Text with a reason — so the user sees a
// "未完成" (orange) card, not a blank green "已完成" one.
func TestEmitCLIEvent_MaxIterations(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:       miniclient.KindResult,
		Text:       "",
		Model:      "kimi",
		Finish:     miniclient.FinishMaxIterations,
		IsTerminal: true,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	got := sender.Controls()
	if len(got) != 1 || got[0].Result == nil {
		t.Fatalf("want one Result, got %+v", got)
	}
	r := got[0].Result
	if !r.Incomplete {
		t.Error("Incomplete = false, want true for max_iterations")
	}
	if r.Text == "" {
		t.Error("Text empty: max_iterations should fill a reason placeholder")
	}
}

// TestEmitCLIEvent_NormalFinish verifies a stop finish keeps Incomplete false
// and the reply text propagated unchanged.
func TestEmitCLIEvent_NormalFinish(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:       miniclient.KindResult,
		Text:       "done",
		Model:      "kimi",
		Finish:     miniclient.FinishStop,
		IsTerminal: true,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	r := sender.Controls()[0].Result
	if r.Incomplete {
		t.Error("Incomplete = true, want false for stop")
	}
	if r.Text != "done" {
		t.Errorf("Text = %q, want \"done\" unchanged", r.Text)
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

// TestEmitCLIEvent_ToolResult_NonShell verifies a v2.0.0 tool_result event for
// a non-shell tool maps to a TypeToolResult with output propagated and is_error
// taken verbatim (no exit_code on non-shell tools).
func TestEmitCLIEvent_ToolResult_NonShell(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:    miniclient.KindToolResult,
		Name:    "read",
		Output:  "file contents",
		IsError: false,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	got := sender.Controls()
	if len(got) != 1 || got[0].ToolResult == nil {
		t.Fatalf("want one ToolResult, got %+v", got)
	}
	tr := got[0].ToolResult
	if tr.Name != "read" || tr.Output != "file contents" {
		t.Errorf("got name=%q output=%q", tr.Name, tr.Output)
	}
	if tr.IsError {
		t.Error("IsError = true, want false")
	}
}

// TestEmitCLIEvent_ToolResult_ShellExitCode covers the v2.0.0 breaking change
// (decision D2): shell non-zero exit reports exit_code with is_error=false. The
// handler prepends [exit N] to the output and keeps IsError false for >0;
// exit_code 0 adds no prefix; exit_code<0 (CLI timeout/startup sentinel) is the
// one case that stays IsError=true.
func TestEmitCLIEvent_ToolResult_ShellExitCode(t *testing.T) {
	cases := []struct {
		name       string
		code       int
		input      string
		wantOutput string
		wantErr    bool
	}{
		{"nonzero", 1, "fail msg", "[exit 1] fail msg", false},
		{"zero", 0, "ok", "ok", false},
		{"timeout", -1, "", "[exit -1]", true},
		{"nonzero-empty-output", 2, "", "[exit 2]", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, sender := newCLIHandler(t)
			code := c.code
			ev := miniclient.Event{
				Kind:     miniclient.KindToolResult,
				Name:     "shell",
				Output:   c.input,
				ExitCode: &code,
			}
			h.emitCLIEvent("c", "p", ev, time.Now())
			got := sender.Controls()
			if len(got) != 1 || got[0].ToolResult == nil {
				t.Fatalf("want one ToolResult, got %+v", got)
			}
			tr := got[0].ToolResult
			if tr.Output != c.wantOutput {
				t.Errorf("Output = %q, want %q", tr.Output, c.wantOutput)
			}
			if tr.IsError != c.wantErr {
				t.Errorf("IsError = %v, want %v", tr.IsError, c.wantErr)
			}
		})
	}
}

// TestEmitCLIEvent_ToolResult_Truncated verifies the truncated suffix is added
// so the user knows the CLI's 2000-char event excerpt elided more.
func TestEmitCLIEvent_ToolResult_Truncated(t *testing.T) {
	h, sender := newCLIHandler(t)
	ev := miniclient.Event{
		Kind:      miniclient.KindToolResult,
		Name:      "grep",
		Output:    "match line",
		Truncated: true,
	}
	h.emitCLIEvent("c", "p", ev, time.Now())
	tr := sender.Controls()[0].ToolResult
	if !strings.Contains(tr.Output, "match line") || !strings.Contains(tr.Output, "已截断") {
		t.Errorf("Output = %q, want body + truncated suffix", tr.Output)
	}
}

// TestEmitCLIEvent_ReasoningDelta verifies a streaming reasoning_delta (v2.0.0,
// -stream only) maps to a TypeThinking control in APPEND mode (Replace=false),
// feeding the live "思考中" zone chunk by chunk.
func TestEmitCLIEvent_ReasoningDelta(t *testing.T) {
	h, sender := newCLIHandler(t)
	h.emitCLIEvent("c", "p", miniclient.Event{
		Kind: miniclient.KindReasoningDelta,
		Step: 1,
		Text: "thinking chunk",
	}, time.Now())
	got := sender.Controls()
	if len(got) != 1 || got[0].Thinking == nil {
		t.Fatalf("want one Thinking, got %+v", got)
	}
	if got[0].Thinking.Delta != "thinking chunk" {
		t.Errorf("Delta = %q, want thinking chunk", got[0].Thinking.Delta)
	}
	if got[0].Thinking.Replace {
		t.Error("Replace = true, want false (append for streaming deltas)")
	}
}

// TestEmitCLIEvent_TextDelta_Dropped verifies text_delta is intentionally NOT
// forwarded: the frontend dispatcher drops TypeText (live text preview was
// removed), and the full reply arrives in the terminal result event. The
// handler emits nothing for it.
func TestEmitCLIEvent_TextDelta_Dropped(t *testing.T) {
	h, sender := newCLIHandler(t)
	h.emitCLIEvent("c", "p", miniclient.Event{
		Kind: miniclient.KindTextDelta,
		Text: "chunk",
	}, time.Now())
	if len(sender.Controls()) != 0 {
		t.Errorf("text_delta should emit no control, got %+v", sender.Controls())
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

	h := New(&captureSender{}, log.Nop(), r, "/global-root", "test-model", nil, 0, "", false)

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
