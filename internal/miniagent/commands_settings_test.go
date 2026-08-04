package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// newSettingsHandler builds a Handler with a persisted router so /mode and
// /thinking can exercise the SetMode/SetThinking → mutate → saveAsync path.
// client is nil: clientDefaultMode/Thinking return the "default"/"off"
// sentinels, which is what the display + clear branches need to assert.
func newSettingsHandler(t *testing.T) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New(filepath.Join(t.TempDir(), "r.json"), log.Nop())
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	h := New(&captureSender{}, log.Nop(), r, "/root", "test-model", nil, "", 0, "", false)
	return h, r
}

// TestCmdMode_ShowDefault verifies /mode with no arg reports the effective
// mode (client default "default" with no client wired).
func TestCmdMode_ShowDefault(t *testing.T) {
	h, _ := newSettingsHandler(t)
	level, _, body := h.cmdMode(context.Background(), "c1", "")
	if level != "info" {
		t.Errorf("level = %q, want info", level)
	}
	if !strings.Contains(body, "default") {
		t.Errorf("body = %q, want it to mention the default mode", body)
	}
}

// TestCmdMode_PinValid verifies /mode <valid> creates the binding, persists the
// pin, and reports success. Subsequent /mode (no arg) shows the pinned value.
func TestCmdMode_PinValid(t *testing.T) {
	h, r := newSettingsHandler(t)
	for _, m := range []string{"default", "auto"} {
		if level, _, _ := h.cmdMode(context.Background(), "c1", m); level != "success" {
			t.Errorf("mode=%s: level = %q, want success", m, level)
		}
		b, _ := r.Lookup("c1")
		if b.Mode != m {
			t.Errorf("after pin mode=%s: binding.Mode = %q", m, b.Mode)
		}
		_, _, body := h.cmdMode(context.Background(), "c1", "")
		if !strings.Contains(body, m) {
			t.Errorf("mode=%s: /mode display body = %q should contain pinned value", m, body)
		}
	}
}

// TestCmdMode_BadValueRejected verifies an out-of-enum value is rejected with
// an error notice AND does NOT create or mutate the binding.
func TestCmdMode_BadValueRejected(t *testing.T) {
	h, r := newSettingsHandler(t)
	level, _, _ := h.cmdMode(context.Background(), "c1", "yolo")
	if level != "error" {
		t.Errorf("level = %q, want error for bad mode", level)
	}
	if _, ok := r.Lookup("c1"); ok {
		t.Error("bad /mode value must not create a binding")
	}
}

// TestCmdMode_Clear verifies /mode clear empties the pin and the body mentions
// the global default. Runs after a pin so ensureBinding already created the
// binding; clear must set the field to "" without dropping the binding itself.
func TestCmdMode_Clear(t *testing.T) {
	h, r := newSettingsHandler(t)
	r.Bind("c1", "", "", "", "", "")
	r.SetMode("c1", "auto")

	level, title, body := h.cmdMode(context.Background(), "c1", "clear")
	if level != "success" || !strings.Contains(title, "默认") {
		t.Errorf("clear = (%q, %q), want success + 默认 in title", level, title)
	}
	b, _ := r.Lookup("c1")
	if b.Mode != "" {
		t.Errorf("after clear: Mode = %q, want empty", b.Mode)
	}
	if !strings.Contains(body, "default") {
		t.Errorf("clear body = %q should mention global default", body)
	}
}

// TestCmdMode_EnsureBindingCreatesOne verifies /mode <valid> on a chat with no
// prior binding creates one (ensureBinding) so SetMode is not a silent no-op.
func TestCmdMode_EnsureBindingCreatesOne(t *testing.T) {
	h, r := newSettingsHandler(t)
	if _, ok := r.Lookup("ghost"); ok {
		t.Fatal("precondition: ghost must not exist")
	}
	h.cmdMode(context.Background(), "ghost", "auto")
	b, ok := r.Lookup("ghost")
	if !ok {
		t.Fatal("ensureBinding must create the binding before SetMode")
	}
	if b.Mode != "auto" {
		t.Errorf("binding.Mode = %q, want auto", b.Mode)
	}
}

// TestCmdThinking_ShowDefault verifies /thinking with no arg reports the
// effective level (client default "off").
func TestCmdThinking_ShowDefault(t *testing.T) {
	h, _ := newSettingsHandler(t)
	level, _, body := h.cmdThinking(context.Background(), "c1", "")
	if level != "info" {
		t.Errorf("level = %q, want info", level)
	}
	if !strings.Contains(body, "off") {
		t.Errorf("body = %q, want it to mention the default level", body)
	}
}

// TestCmdThinking_PinValid verifies /thinking <valid> pins each accepted level.
func TestCmdThinking_PinValid(t *testing.T) {
	h, r := newSettingsHandler(t)
	for _, lvl := range []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if level, _, _ := h.cmdThinking(context.Background(), "c1", lvl); level != "success" {
			t.Errorf("lvl=%s: level = %q, want success", lvl, level)
		}
		b, _ := r.Lookup("c1")
		if b.Thinking != lvl {
			t.Errorf("after pin lvl=%s: binding.Thinking = %q", lvl, b.Thinking)
		}
	}
}

// TestCmdThinking_BadValueRejected verifies an out-of-enum value is rejected
// without creating a binding.
func TestCmdThinking_BadValueRejected(t *testing.T) {
	h, r := newSettingsHandler(t)
	// "auto" is NOT in settableThinkingLevels (miniagent v3 has no auto).
	level, _, _ := h.cmdThinking(context.Background(), "c1", "auto")
	if level != "error" {
		t.Errorf("level = %q, want error for auto (miniagent has no auto)", level)
	}
	if _, ok := r.Lookup("c1"); ok {
		t.Error("bad /thinking value must not create a binding")
	}
}

// TestCmdThinking_Clear verifies /thinking clear empties the pin and reports
// the global default.
func TestCmdThinking_Clear(t *testing.T) {
	h, r := newSettingsHandler(t)
	r.Bind("c1", "", "", "", "", "")
	r.SetThinking("c1", "high")

	level, _, body := h.cmdThinking(context.Background(), "c1", "clear")
	if level != "success" {
		t.Errorf("level = %q, want success", level)
	}
	b, _ := r.Lookup("c1")
	if b.Thinking != "" {
		t.Errorf("after clear: Thinking = %q, want empty", b.Thinking)
	}
	if !strings.Contains(body, "off") {
		t.Errorf("clear body = %q should mention global default off", body)
	}
}

// TestCmdMaxIter_ShowDefault verifies /maxiter with no arg reports the effective
// cap. With no client wired the default is 0 → "默认（上游 CLI，约 20）".
func TestCmdMaxIter_ShowDefault(t *testing.T) {
	h, _ := newSettingsHandler(t)
	level, _, body := h.cmdMaxIter(context.Background(), "c1", "")
	if level != "info" {
		t.Errorf("level = %q, want info", level)
	}
	if !strings.Contains(body, "默认") {
		t.Errorf("body = %q, want it to mention the default (upstream ~20)", body)
	}
}

// TestCmdMaxIter_PinValid verifies /maxiter <N> pins N, persists it, reports
// success, and a subsequent /maxiter (no arg) shows the pinned value.
func TestCmdMaxIter_PinValid(t *testing.T) {
	h, r := newSettingsHandler(t)
	level, _, _ := h.cmdMaxIter(context.Background(), "c1", "50")
	if level != "success" {
		t.Errorf("level = %q, want success", level)
	}
	b, _ := r.Lookup("c1")
	if b.MaxIterations != 50 {
		t.Errorf("binding.MaxIterations = %d, want 50", b.MaxIterations)
	}
	_, _, body := h.cmdMaxIter(context.Background(), "c1", "")
	if !strings.Contains(body, "50") {
		t.Errorf("/maxiter display = %q should contain pinned 50", body)
	}
}

// TestCmdMaxIter_BadValueRejected verifies <1 and non-numeric args are rejected
// with an error AND do NOT create a binding (ensureBinding runs only on the
// success paths, mirroring cmdMode).
func TestCmdMaxIter_BadValueRejected(t *testing.T) {
	h, r := newSettingsHandler(t)
	for _, bad := range []string{"0", "-1", "abc", "1.5"} {
		level, _, _ := h.cmdMaxIter(context.Background(), "c1", bad)
		if level != "error" {
			t.Errorf("arg=%q: level = %q, want error", bad, level)
		}
		if _, ok := r.Lookup("c1"); ok {
			t.Errorf("arg=%q: bad /maxiter must not create a binding", bad)
		}
	}
}

// TestCmdMaxIter_Clear verifies /maxiter clear zeroes the pin (0 = unset) and
// the body mentions the global default. Runs after an explicit pin so the
// binding exists; clear must set the field to 0 without dropping the binding.
func TestCmdMaxIter_Clear(t *testing.T) {
	h, r := newSettingsHandler(t)
	r.Bind("c1", "", "", "", "", "")
	r.SetMaxIterations("c1", 50)

	level, _, body := h.cmdMaxIter(context.Background(), "c1", "clear")
	if level != "success" {
		t.Errorf("level = %q, want success", level)
	}
	b, _ := r.Lookup("c1")
	if b.MaxIterations != 0 {
		t.Errorf("after clear: MaxIterations = %d, want 0", b.MaxIterations)
	}
	if !strings.Contains(body, "默认") {
		t.Errorf("clear body = %q should mention global default", body)
	}
}

// --- Phase 3: /new (per-chat session jsonl deletion, R2) ---

// newNewHandler builds a Handler with a real stateDir so sessionRoot is
// non-empty and /new has a directory to delete from. client/router stay nil:
// cmdNew only needs sessionPath, not the binding or fork path.
func newNewHandler(t *testing.T) *Handler {
	t.Helper()
	h := New(&captureSender{}, log.Nop(), nil, "", "test-model", nil, "", 0, t.TempDir(), false)
	return h
}

// TestCmdNew_DeletesExistingFile verifies /new removes the chat's session
// jsonl so the next prompt starts a fresh conversation (R2). Writes a sentinel
// file at the sha256-hashed path, then /new must delete it and report success.
func TestCmdNew_DeletesExistingFile(t *testing.T) {
	h := newNewHandler(t)
	p := h.sessionPath("oc_chat_1")
	if p == "" {
		t.Fatal("precondition: sessionPath must be non-empty with stateDir set")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(`{"role":"user","content":"hi"}`), 0o600); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	level, title, body := h.cmdNew(context.Background(), "oc_chat_1", "")
	if level != "success" {
		t.Errorf("level = %q, want success", level)
	}
	if !strings.Contains(title, "清除") {
		t.Errorf("title = %q, want 清除 in title", title)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("after /new: stat session file = %v, want IsNotExist", err)
	}
	_ = body // body text asserted in the no-op test below
}

// TestCmdNew_MissingFileIsNoOp verifies /new on a chat with no session file
// (first prompt ever, or already cleared) is still a success — there is nothing
// to forget, and the user should not see a scary error.
func TestCmdNew_MissingFileIsNoOp(t *testing.T) {
	h := newNewHandler(t)
	p := h.sessionPath("oc_never")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// No file seeded.
	level, _, body := h.cmdNew(context.Background(), "oc_never", "")
	if level != "success" {
		t.Errorf("level = %q, want success (missing file is a no-op)", level)
	}
	if !strings.Contains(body, "新会话") {
		t.Errorf("body = %q, want it to mention starting a new session", body)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("after /new on missing: file now exists (unexpected): %v", err)
	}
}

// TestCmdNew_EmptySessionRootWarns verifies /new with no sessionRoot
// configured (stateDir empty) returns a warning, not a silent success: the
// operator likely misconfigured stateDir, and clearing "nothing" would hide that.
func TestCmdNew_EmptySessionRootWarns(t *testing.T) {
	h := New(&captureSender{}, log.Nop(), nil, "", "test-model", nil, "", 0, "", false)
	level, title, body := h.cmdNew(context.Background(), "oc_x", "")
	if level != "warning" {
		t.Errorf("level = %q, want warning when sessionRoot unset", level)
	}
	if !strings.Contains(title, "清除") {
		t.Errorf("title = %q, want 清除 in title", title)
	}
	if !strings.Contains(body, "未配置") {
		t.Errorf("body = %q, want it to mention the dir is unconfigured", body)
	}
}
