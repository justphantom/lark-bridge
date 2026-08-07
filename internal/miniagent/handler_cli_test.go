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
	h := New(sender, log.Nop(), nil, "", "test-model", "", nil, "", 0, "", false)
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
	model, _, dir, mode, thinking, _ := h.activeTurnConfig("c1")
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

	h := New(&captureSender{}, log.Nop(), r, "/global-root", "test-model", "", nil, "", 0, "", false)

	if model, _, _, _, _, _ := h.activeTurnConfig("c1"); model != "kimi" {
		t.Errorf("bound model = %q, want kimi", model)
	}
	if _, _, dir, _, _, _ := h.activeTurnConfig("c1"); dir != "/proj" {
		t.Errorf("bound dir = %q, want /proj", dir)
	}

	// A chat without a binding still gets the global defaults — proves the
	// override is per-chat, not process-wide.
	if model, _, dir, _, _, _ := h.activeTurnConfig("no-such-chat"); model != "test-model" || dir != "/global-root" {
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

	h := New(&captureSender{}, log.Nop(), r, "/root", "m", "", nil, "", 0, "", false)

	// Default effective values without any pin.
	if _, _, _, mode, thinking, _ := h.activeTurnConfig("c1"); mode != "default" || thinking != "off" {
		t.Errorf("defaults = (%q, %q), want (default, off)", mode, thinking)
	}

	r.SetMode("c1", "auto")
	r.SetThinking("c1", "high")
	if _, _, _, mode, thinking, _ := h.activeTurnConfig("c1"); mode != "auto" || thinking != "high" {
		t.Errorf("pinned = (%q, %q), want (auto, high)", mode, thinking)
	}

	// Clearing the pin returns the global default (no per-chat value).
	r.SetMode("c1", "")
	r.SetThinking("c1", "")
	if _, _, _, mode, thinking, _ := h.activeTurnConfig("c1"); mode != "default" || thinking != "off" {
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
	h := New(&captureSender{}, log.Nop(), r, "/root", "m", "", nil, "", 0, "", false)

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

// --- Phase 3: per-chat session (sessionIDFile / -session/-save-session wiring / R4) ---

// newSessionHandler builds a Handler whose stateDir (and thus sessionRoot) is a
// real temp dir, so sessionIDFile returns non-empty absolute paths. client is nil
// by default; tests that need the CLI fork path wire a stub via setStubClient.
func newSessionHandler(t *testing.T) (*Handler, string) {
	t.Helper()
	stateDir := t.TempDir()
	h := New(&captureSender{}, log.Nop(), nil, "", "test-model", "", nil, "", 0, stateDir, false)
	return h, stateDir
}

// TestSessionIDFile_Deterministic verifies the same chatID resolves to the same
// mapping path (so the chatID→sessionID mapping is stable across turns), and
// distinct chatIDs resolve to distinct paths (no cross-chat collision).
func TestSessionIDFile_Deterministic(t *testing.T) {
	h, _ := newSessionHandler(t)
	p1 := h.sessionIDFile("oc_chat_a")
	p2 := h.sessionIDFile("oc_chat_a")
	p3 := h.sessionIDFile("oc_chat_b")
	if p1 != p2 {
		t.Errorf("same chatID must be stable: %q vs %q", p1, p2)
	}
	if p1 == p3 {
		t.Errorf("distinct chatIDs must differ: both %q", p1)
	}
	// Path must equal sha256(chatID).id under sessionRoot — the documented
	// contract, so an operator can map a chatID to its mapping file by hand.
	sum := sha256.Sum256([]byte("oc_chat_a"))
	want := filepath.Join(h.sessionRoot, hex.EncodeToString(sum[:])+".id")
	if p1 != want {
		t.Errorf("path = %q, want %q", p1, want)
	}
}

// TestSessionIDFile_PathSafety verifies a malicious chatID cannot escape
// sessionRoot: chatIDs containing "..", "/", drive letters, or NUL must all
// hash to a flat filename under sessionRoot. This is the R4/path-traversal
// guard called out in §3.2 and §6 of the implementation manual.
func TestSessionIDFile_PathSafety(t *testing.T) {
	h, _ := newSessionHandler(t)
	for _, bad := range []string{
		"../../../etc/passwd", "..", "/", "a/b/../../../c",
		"\x00", "C:\\windows\\system32", "....//....//etc",
	} {
		p := h.sessionIDFile(bad)
		if p == "" {
			t.Errorf("chatID=%q: path empty for non-empty sessionRoot", bad)
		}
		if !strings.HasPrefix(p, h.sessionRoot+string(filepath.Separator)) {
			t.Errorf("chatID=%q escaped sessionRoot: %q (root=%q)", bad, p, h.sessionRoot)
		}
		// The base name must be exactly 64 hex chars + ".id" — no chatID
		// bytes leaked into the path.
		base := filepath.Base(p)
		wantSuffix := ".id"
		if !strings.HasSuffix(base, wantSuffix) {
			t.Errorf("chatID=%q: base %q must end with %q", bad, base, wantSuffix)
		}
		hexPart := strings.TrimSuffix(base, wantSuffix)
		if len(hexPart) != 64 {
			t.Errorf("chatID=%q: hex part len=%d, want 64 (sha256)", bad, len(hexPart))
		}
	}
}

// TestSessionIDFile_EmptyRootStateless verifies that when stateDir is unset
// (some tests, or a misconfigured deploy) sessionIDFile returns "" →
// lookupSessionID returns "" → runViaCLI runs a stateless turn (no -session /
// -save-session). This is the graceful-degrade path.
func TestSessionIDFile_EmptyRootStateless(t *testing.T) {
	h := New(&captureSender{}, log.Nop(), nil, "", "m", "", nil, "", 0, "", false)
	if got := h.sessionIDFile("any-chat"); got != "" {
		t.Errorf("empty sessionRoot: sessionIDFile = %q, want empty", got)
	}
}

// TestRunViaCLI_PassesSaveSession verifies the first turn for a chat (no
// persisted session id) is forked with -save-session, and that the id miniagent
// emits as a stdout type=session event is persisted to the chat's mapping file.
// Drives a real miniclient.Client whose cliPath is a stub shell script: capture
// argv, emit a session event (id), then a terminal result.
//
// This is the strongest feasible coverage without refactoring Handler.client
// (a concrete *miniclient.Client) to an interface, which §3 does not list.
func TestRunViaCLI_PassesSaveSession(t *testing.T) {
	stateDir := t.TempDir()
	captureFile := filepath.Join(stateDir, "argv")
	stub := filepath.Join(stateDir, "stub.sh")
	// Stub: capture argv, emit a session event (the id to persist), then a
	// terminal result so the pump sees IsTerminal and closes.
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > ` + captureFile + "\n" +
		`printf '{"type":"session","id":"stub-session-id"}\n'` + "\n" +
		`printf '{"type":"result","text":"ok","model":"stub","steps":1}\n'` + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	client := miniclient.New(miniclient.Config{CLIPath: stub, APIKey: "k"}, log.Nop())
	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, "", "test-model", "", client, "", 0, stateDir, false)
	defer h.Close()

	mapPath := h.sessionIDFile("oc_chat_1")
	if mapPath == "" {
		t.Fatal("precondition: sessionIDFile must be non-empty with stateDir set")
	}

	h.runViaCLI(context.Background(), "p1", "oc_chat_1", "hello")

	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture: %v (did the stub run?)", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// First turn (no mapping) → -save-session, NOT -session.
	if !argvHas(lines, "-save-session") {
		t.Errorf("-save-session missing from argv: %v", lines)
	}
	if argvHas(lines, "-session") {
		t.Errorf("-session must not appear on first turn: %v", lines)
	}
	// The emitted session id must be persisted to the mapping file.
	gotID, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("mapping file not written: %v", err)
	}
	if strings.TrimSpace(string(gotID)) != "stub-session-id" {
		t.Errorf("persisted id = %q, want stub-session-id", gotID)
	}
	if !harnessHasResult(sender) {
		t.Error("expected one TypeResult control after runViaCLI")
	}
}

// TestRunViaCLI_ResumesWithSessionID verifies that once a session id is
// persisted (a prior -save-session turn), the next turn forks with
// -session <id> (resume), not -save-session.
func TestRunViaCLI_ResumesWithSessionID(t *testing.T) {
	stateDir := t.TempDir()
	captureFile := filepath.Join(stateDir, "argv")
	stub := filepath.Join(stateDir, "stub.sh")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$@" > ` + captureFile + "\n" +
		`printf '{"type":"result","text":"ok","model":"stub","steps":1}\n'` + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	client := miniclient.New(miniclient.Config{CLIPath: stub, APIKey: "k"}, log.Nop())
	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, "", "test-model", "", client, "", 0, stateDir, false)
	defer h.Close()

	// Seed the mapping as a prior turn would have persisted it.
	if err := os.WriteFile(h.sessionIDFile("oc_chat_1"), []byte("prior-id"), 0o600); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	h.runViaCLI(context.Background(), "p1", "oc_chat_1", "hello")

	data, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatalf("read capture: %v (did the stub run?)", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	// Resume → -session prior-id, NOT -save-session.
	if v := argvValue(lines, "-session"); v != "prior-id" {
		t.Errorf("-session = %q, want prior-id (argv=%v)", v, lines)
	}
	if argvHas(lines, "-save-session") {
		t.Errorf("-save-session must not appear on resume: %v", lines)
	}
	if !harnessHasResult(sender) {
		t.Error("expected one TypeResult control after runViaCLI")
	}
}

// argvHas reports whether flag appears as its own line in a captured argv dump
// (printf '%s\n' "$@"). Needed because "-session" is a substring of
// "-save-session" — a plain substring check would conflate them.
func argvHas(lines []string, flag string) bool {
	for _, ln := range lines {
		if ln == flag {
			return true
		}
	}
	return false
}

// argvValue returns the argument following flag in a captured argv dump, or "".
func argvValue(lines []string, flag string) string {
	for i, ln := range lines {
		if ln == flag && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
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
	turnCtx, mine, ok := h.startTurn(ctx, "c", "p1")
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

	got := filterNotices(sender.Controls())
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

	turnCtx, mine, ok := h.startTurn(context.Background(), "c", "p1")
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
	notices := filterNotices(sender.Controls())
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

// filterNotices strips turn-lifecycle controls emitted by startTurn/endTurn so
// busy-drop tests see only the notices they assert against.
func filterNotices(ctrls []*protocol.Control) []*protocol.Control {
	out := make([]*protocol.Control, 0, len(ctrls))
	for _, c := range ctrls {
		if c.Type == protocol.TypeNotice {
			out = append(out, c)
		}
	}
	return out
}

