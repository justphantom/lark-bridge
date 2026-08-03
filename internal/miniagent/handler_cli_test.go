package miniagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/protocol"
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
// global defaults (cfgModel, workspaceRoot) are returned. Mode/Thinking fall
// back to the client-default sentinels ("default"/"off") since no client is
// wired in newCLIHandler.
func TestActiveTurnConfig_DefaultsNoBinding(t *testing.T) {
	h, _ := newCLIHandler(t)
	model, dir, mode, thinking := h.activeTurnConfig("c1")
	if model != "test-model" {
		t.Errorf("model = %q, want test-model", model)
	}
	if dir != "" {
		t.Errorf("dir = %q, want empty (no workspaceRoot configured)", dir)
	}
	if mode != "default" {
		t.Errorf("mode = %q, want default (client nil)", mode)
	}
	if thinking != "off" {
		t.Errorf("thinking = %q, want off (client nil)", thinking)
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

	if model, _, _, _ := h.activeTurnConfig("c1"); model != "kimi" {
		t.Errorf("bound model = %q, want kimi", model)
	}
	if _, dir, _, _ := h.activeTurnConfig("c1"); dir != "/proj" {
		t.Errorf("bound dir = %q, want /proj", dir)
	}

	// A chat without a binding still gets the global defaults — proves the
	// override is per-chat, not process-wide.
	if model, dir, _, _ := h.activeTurnConfig("no-such-chat"); model != "test-model" || dir != "/global-root" {
		t.Errorf("unbound = (%q, %q), want (test-model, /global-root)", model, dir)
	}
}

// TestActiveTurnConfig_PerChatModeThinkingOverride verifies per-chat Mode and
// Thinking pins (set by /mode and /thinking via SetMode/SetThinking) override
// the client defaults returned by activeTurnConfig. With no client wired the
// defaults are "default"/"off"; pinning "auto"/"high" must surface through the
// 4-value return so runViaCLI's RunOptions carries the per-chat value.
func TestActiveTurnConfig_PerChatModeThinkingOverride(t *testing.T) {
	r, err := router.New(filepath.Join(t.TempDir(), "r.json"), log.Nop())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	defer r.Close()
	r.Bind("c1", "", "", "", "", "")

	h := New(&captureSender{}, log.Nop(), r, "/root", "m", nil, 0, "", false)

	// Default effective values without any pin.
	if _, _, mode, thinking := h.activeTurnConfig("c1"); mode != "default" || thinking != "off" {
		t.Errorf("defaults = (%q, %q), want (default, off)", mode, thinking)
	}

	r.SetMode("c1", "auto")
	r.SetThinking("c1", "high")
	if _, _, mode, thinking := h.activeTurnConfig("c1"); mode != "auto" || thinking != "high" {
		t.Errorf("pinned = (%q, %q), want (auto, high)", mode, thinking)
	}

	// Clearing the pin returns the global default (no per-chat value).
	r.SetMode("c1", "")
	r.SetThinking("c1", "")
	if _, _, mode, thinking := h.activeTurnConfig("c1"); mode != "default" || thinking != "off" {
		t.Errorf("after clear = (%q, %q), want (default, off)", mode, thinking)
	}
}

// TestActiveMaxIter_PerChatOverrideAndDefault verifies activeMaxIter returns the
// per-chat pin (>0) when set, and falls back to the client default (0 with no
// client wired) otherwise — same precedence shape as activeMode/activeThinking.
// 0 is the clear value: a pinned 0 must NOT shadow the client default.
func TestActiveMaxIter_PerChatOverrideAndDefault(t *testing.T) {
	r, err := router.New(filepath.Join(t.TempDir(), "r.json"), log.Nop())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	defer r.Close()
	r.Bind("c1", "", "", "", "", "")
	h := New(&captureSender{}, log.Nop(), r, "/root", "m", nil, 0, "", false)

	// No pin → client default (client nil → 0).
	if got := h.activeMaxIter("c1"); got != 0 {
		t.Errorf("default activeMaxIter = %d, want 0", got)
	}
	// Pin > 0 surfaces verbatim.
	r.SetMaxIterations("c1", 50)
	if got := h.activeMaxIter("c1"); got != 50 {
		t.Errorf("pinned activeMaxIter = %d, want 50", got)
	}
	// A pinned 0 (= clear) falls back to the client default again — this is
	// why activeMaxIter gates on >0, not != 0.
	r.SetMaxIterations("c1", 0)
	if got := h.activeMaxIter("c1"); got != 0 {
		t.Errorf("after clear activeMaxIter = %d, want 0", got)
	}
	// Unbound chat → client default.
	if got := h.activeMaxIter("no-such-chat"); got != 0 {
		t.Errorf("unbound activeMaxIter = %d, want 0", got)
	}
}

// --- Phase 3: per-chat session (sessionPath / -session wiring / R4) ---

// newSessionHandler builds a Handler whose stateDir (and thus sessionRoot) is a
// real temp dir, so sessionPath returns non-empty absolute paths. client is nil
// by default; tests that need the CLI fork path wire a stub via setStubClient.
func newSessionHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	stateDir := t.TempDir()
	h := New(&captureSender{}, log.Nop(), nil, "", "test-model", nil, 0, stateDir, false)
	return h, stateDir
}

// TestSessionPath_Deterministic verifies the same chatID resolves to the same
// jsonl path (so miniagent re-feeds the same conversation across turns), and
// distinct chatIDs resolve to distinct paths (no cross-chat collision).
func TestSessionPath_Deterministic(t *testing.T) {
	h, _ := newSessionHandler(t)
	p1 := h.sessionPath("oc_chat_a")
	p2 := h.sessionPath("oc_chat_a")
	p3 := h.sessionPath("oc_chat_b")
	if p1 != p2 {
		t.Errorf("same chatID must be stable: %q vs %q", p1, p2)
	}
	if p1 == p3 {
		t.Errorf("distinct chatIDs must differ: both %q", p1)
	}
	// Path must equal sha256(chatID).jsonl under sessionRoot — the documented
	// contract, so an operator can map a chatID to its file by hand if needed.
	sum := sha256.Sum256([]byte("oc_chat_a"))
	want := filepath.Join(h.sessionRoot, hex.EncodeToString(sum[:])+".jsonl")
	if p1 != want {
		t.Errorf("path = %q, want %q", p1, want)
	}
}

// TestSessionPath_PathSafety verifies a malicious chatID cannot escape
// sessionRoot: chatIDs containing "..", "/", drive letters, or NUL must all
// hash to a flat filename under sessionRoot. This is the R4/path-traversal
// guard called out in §3.2 and §6 of the implementation manual.
func TestSessionPath_PathSafety(t *testing.T) {
	h, _ := newSessionHandler(t)
	for _, bad := range []string{
		"../../../etc/passwd", "..", "/", "a/b/../../../c",
		"\x00", "C:\\windows\\system32", "....//....//etc",
	} {
		p := h.sessionPath(bad)
		if p == "" {
			t.Errorf("chatID=%q: path empty for non-empty sessionRoot", bad)
		}
		if !strings.HasPrefix(p, h.sessionRoot+string(filepath.Separator)) {
			t.Errorf("chatID=%q escaped sessionRoot: %q (root=%q)", bad, p, h.sessionRoot)
		}
		// The base name must be exactly 64 hex chars + ".jsonl" — no chatID
		// bytes leaked into the path.
		base := filepath.Base(p)
		wantSuffix := ".jsonl"
		if !strings.HasSuffix(base, wantSuffix) {
			t.Errorf("chatID=%q: base %q must end with %q", bad, base, wantSuffix)
		}
		hexPart := strings.TrimSuffix(base, wantSuffix)
		if len(hexPart) != 64 {
			t.Errorf("chatID=%q: hex part len=%d, want 64 (sha256)", bad, len(hexPart))
		}
	}
}

// TestSessionPath_EmptyRootStateless verifies that when stateDir is unset (some
// tests, or a misconfigured deploy) sessionPath returns "" → runViaCLI passes
// "" → buildArgs omits -session → miniagent runs a stateless turn. This is the
// graceful-degrade path.
func TestSessionPath_EmptyRootStateless(t *testing.T) {
	h := New(&captureSender{}, log.Nop(), nil, "", "m", nil, 0, "", false)
	if got := h.sessionPath("any-chat"); got != "" {
		t.Errorf("empty sessionRoot: sessionPath = %q, want empty", got)
	}
}

// TestRunViaCLI_PassesSessionArg verifies runViaCLI wires
// h.sessionPath(chatID) into RunOptions.Session by driving a real
// miniclient.Client whose cliPath is a stub shell script. The script captures
// its argv to a file and emits one valid result event so runViaCLI completes
// cleanly. We then assert the captured argv contains "-session <expected path>".
//
// This is the strongest feasible coverage without refactoring Handler.client
// (a concrete *miniclient.Client) to an interface, which §3 does not list.
func TestRunViaCLI_PassesSessionArg(t *testing.T) {
	stateDir := t.TempDir()
	captureFile := filepath.Join(stateDir, "argv")
	// Stub binary: write "$@" to captureFile, then emit one terminal result
	// event on stdout so the miniclient pump sees IsTerminal and closes.
	stub := filepath.Join(stateDir, "stub.sh")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > ` + captureFile + "\n" +
		`printf '{"type":"result","text":"ok","model":"stub","steps":1}\n'` + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	client := miniclient.New(miniclient.Config{CLIPath: stub, APIKey: "k"}, log.Nop())
	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, "", "test-model", client, 0, stateDir, false)
	defer h.Close()

	wantPath := h.sessionPath("oc_chat_1")
	if wantPath == "" {
		t.Fatal("precondition: sessionPath must be non-empty with stateDir set")
	}

	h.runViaCLI(context.Background(), "p1", "oc_chat_1", "hello")

	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture: %v (did the stub run?)", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// Find "-session" and assert the following arg is the expected abspath.
	for i, ln := range lines {
		if ln == "-session" && i+1 < len(lines) {
			if lines[i+1] != wantPath {
				t.Errorf("-session arg = %q, want %q", lines[i+1], wantPath)
			}
			// Confirm a Result control landed — proves runViaCLI drained the
			// event stream to completion.
			if !harnessHasResult(sender) {
				t.Error("expected one TypeResult control after runViaCLI")
			}
			return
		}
	}
	t.Errorf("-session flag missing from captured argv: %v", lines)
}

// harnessHasResult reports whether sender captured a TypeResult control.
func harnessHasResult(sender *captureSender) bool {
	for _, c := range sender.Controls() {
		if c.Type == protocol.TypeResult {
			return true
		}
	}
	return false
}

// TestR4_SecondPromptWhileBusyIsDropped is the R4 regression test (§3.4). R4
// (same-chat jsonl writes serialised) is satisfied by startTurn's busy-then-drop:
// a chat with an in-flight turn rejects a new prompt with a "处理中" notice and
// does NOT fork a second miniagent process. We occupy the slot with startTurn,
// fire a second prompt via HandleEvent, and assert (a) a warning notice with the
// "处理中" body and (b) no terminal result from a second turn. The slot is
// released cleanly at the end.
func TestR4_SecondPromptWhileBusyIsDropped(t *testing.T) {
	h, sender := newTestHandler()
	defer h.Close()

	// Occupy chat "c"'s turn slot — simulates an in-flight runTurn whose
	// miniagent subprocess is still writing its session jsonl.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	turnCtx, mine, ok := h.startTurn(ctx, "c")
	if !ok {
		t.Fatal("precondition: first startTurn must win the slot")
	}
	_ = turnCtx
	defer h.endTurn("c", mine)

	// Fire a second prompt for the SAME chat. HandleEvent must reject it
	// (busy-then-drop), not start a second concurrent fork.
	ev := &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "p2",
		Prompt:   &protocol.PromptPayload{ChatID: "c", Text: "second"},
	}
	if err := h.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	got := sender.Controls()
	if len(got) != 1 {
		t.Fatalf("emits = %d, want exactly 1 (the busy notice, no second turn)", len(got))
	}
	c := got[0]
	if c.Type != protocol.TypeNotice {
		t.Errorf("control type = %v, want Notice", c.Type)
	}
	if c.Notice == nil {
		t.Fatal("Notice payload nil")
	}
	if c.Notice.Level != "warning" {
		t.Errorf("notice level = %q, want warning", c.Notice.Level)
	}
	// The busy title is "处理中"; the body says "还在处理…". Assert on the
	// title (the canonical busy marker); also accept "处理" in the body as a
	// fallback so a future wording tweak does not break the regression guard.
	if !strings.Contains(c.Notice.Title, "处理中") && !strings.Contains(c.Notice.Message, "处理") {
		t.Errorf("notice title=%q body=%q, want 处理中 in title or 处理 in body", c.Notice.Title, c.Notice.Message)
	}
	// The second prompt's promptID must be on the notice so the frontend can
	// resolve its placeholder card.
	if c.PromptID != "p2" {
		t.Errorf("notice PromptID = %q, want p2", c.PromptID)
	}
	// Sanity: still only one turn registered (the one we hold).
	if n := len(h.cancelBy); n != 1 {
		t.Errorf("cancelBy size = %d, want 1 (second prompt must not register a turn)", n)
	}
}

// TestR4_ConcurrentSecondPromptIsDropped is the goroutine-racing variant: hold
// the slot from one goroutine while another fires HandleEvent, and assert no
// second turn is ever registered. Guards against a regression that re-opens
// the slot mid-turn under load.
func TestR4_ConcurrentSecondPromptIsDropped(t *testing.T) {
	h, sender := newTestHandler()
	defer h.Close()

	turnCtx, mine, ok := h.startTurn(context.Background(), "c")
	if !ok {
		t.Fatal("precondition: startTurn must win")
	}
	_ = turnCtx

	var wg sync.WaitGroup
	const N = 8
	wg.Add(N)
	for i := range N {
		go func(i int) {
			defer wg.Done()
			ev := &protocol.Event{
				Type:     protocol.TypePrompt,
				PromptID: "p",
				Prompt:   &protocol.PromptPayload{ChatID: "c", Text: "concurrent"},
			}
			_ = h.HandleEvent(context.Background(), ev)
			_ = i
		}(i)
	}
	wg.Wait()

	// Every one of the N prompts must have been dropped as busy (warning
	// notices), and the cancelBy map must still hold exactly our one turn.
	notices := sender.Controls()
	if len(notices) != N {
		t.Errorf("emits = %d, want %d busy notices", len(notices), N)
	}
	for _, c := range notices {
		if c.Type != protocol.TypeNotice || c.Notice == nil || c.Notice.Level != "warning" {
			t.Errorf("want warning notice, got %+v", c)
		}
	}
	if n := len(h.cancelBy); n != 1 {
		t.Errorf("cancelBy size = %d after concurrent prompts, want 1", n)
	}
	h.endTurn("c", mine)
}

// TestCmdMemory_NoMemorySurface pins the empty-memory path: when no
// .miniagent/memory.jsonl exists in the active workdir, cmdMemory returns the
// "暂无记忆" info card rather than an error.
func TestCmdMemory_NoMemorySurface(t *testing.T) {
	dir := t.TempDir()
	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, dir, "test-model", nil, 0, "", false)
	defer h.Close()

	// No memory file exists; cmdMemory should surface the empty notice.
	level, title, body := h.cmdMemory(context.Background(), "c", "")
	if level != "info" {
		t.Errorf("level = %q, want info", level)
	}
	if title != "项目记忆" {
		t.Errorf("title = %q, want 项目记忆", title)
	}
	if !strings.Contains(body, "暂无记忆") {
		t.Errorf("body = %q, want '暂无记忆' notice", body)
	}
}

// TestCmdMemory_WithRecords renders existing memory records. Writes a valid
// NDJSON memory file and asserts each record appears in the rendered body.
func TestCmdMemory_WithRecords(t *testing.T) {
	dir := t.TempDir()
	memDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	memFile := filepath.Join(memDir, "memory.jsonl")
	records := []string{
		`{"type":"fact","topic":"project","content":"test fact"}`,
		`{"type":"rule","content":"a rule without topic"}`,
	}
	if err := os.WriteFile(memFile, []byte(strings.Join(records, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write memory: %v", err)
	}

	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, dir, "test-model", nil, 0, "", false)
	defer h.Close()

	level, title, body := h.cmdMemory(context.Background(), "c", "")
	if level != "info" {
		t.Errorf("level = %q, want info", level)
	}
	if title != "项目记忆" {
		t.Errorf("title = %q, want 项目记忆", title)
	}
	if !strings.Contains(body, "test fact") {
		t.Errorf("body missing 'test fact': %q", body)
	}
	if !strings.Contains(body, "a rule without topic") {
		t.Errorf("body missing rule content: %q", body)
	}
}

// TestReadMemoryRecords_HomeFallback pins the v3.3.0 dual-layer discovery
// (1ac831e): when <workdir>/.miniagent/memory.jsonl is ABSENT, the home layer
// (~/.miniagent/memory.jsonl) is the fallback. Before this alignment the
// bridge reported "暂无记忆" while the agent itself had home memory injected
// into its system prompt — a visible inconsistency. The workdir layer
// overrides (not merges) the home layer, so a workdir file shadows the home
// file entirely (covered by TestCmdMemory_WithRecords above, where the
// workdir temp dir has no HOME-relative memory).
func TestReadMemoryRecords_HomeFallback(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home) // readMemoryRecords falls back to os.UserHomeDir

	// No workdir memory file; home has one record.
	memDir := filepath.Join(home, ".miniagent")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	memFile := filepath.Join(memDir, "memory.jsonl")
	want := `{"type":"fact","topic":"global","content":"home-layer fact"}`
	if err := os.WriteFile(memFile, []byte(want+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// workdir is a non-empty temp dir WITHOUT a .miniagent/memory.jsonl →
	// the home layer must be returned.
	workdir := t.TempDir()
	got, err := readMemoryRecords(workdir)
	if err != nil {
		t.Fatalf("readMemoryRecords: %v", err)
	}
	if len(got) != 1 || got[0].Content != "home-layer fact" {
		t.Errorf("got %+v, want one home-layer record", got)
	}
}

// TestReadMemoryRecords_WorkdirOverridesHome pins the override-not-merge
// semantics: a workdir memory file shadows the home file completely. Even
// if the home file has more records, only the workdir layer is returned
// (matches upstream loadProjectRules's per-file override).
func TestReadMemoryRecords_WorkdirOverridesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	homeMem := filepath.Join(home, ".miniagent", "memory.jsonl")
	if err := os.MkdirAll(filepath.Dir(homeMem), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(homeMem, []byte(`{"type":"fact","content":"home-only"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write home: %v", err)
	}

	workdir := t.TempDir()
	wdMem := filepath.Join(workdir, ".miniagent", "memory.jsonl")
	if err := os.MkdirAll(filepath.Dir(wdMem), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(wdMem, []byte(`{"type":"rule","content":"workdir-wins"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write workdir: %v", err)
	}

	got, err := readMemoryRecords(workdir)
	if err != nil {
		t.Fatalf("readMemoryRecords: %v", err)
	}
	if len(got) != 1 || got[0].Content != "workdir-wins" {
		t.Errorf("got %+v, want only the workdir record (override, not merge)", got)
	}
}

// TestReadMemoryRecords_NeitherExists returns nil-nil when neither the workdir
// nor the home memory file is present — cmdMemory surfaces the friendly
// "暂无记忆" notice rather than an error card.
func TestReadMemoryRecords_NeitherExists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := readMemoryRecords(t.TempDir())
	if err != nil {
		t.Fatalf("readMemoryRecords: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil when no memory file exists", got)
	}
}
