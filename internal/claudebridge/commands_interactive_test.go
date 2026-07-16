package claudebridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/claude"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/router"
)

// newPickerTestHandler builds a Handler wired with a real in-memory router and
// the given option lists. rpc stays nil (emit is a no-op); the test reads the
// requestID straight from h.answers and delivers the answer itself,
// exercising the same routing the IPC path would.
func newPickerTestHandler(t *testing.T, modelOpts, permOpts, effortOpts []string) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New(nil, "", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, nil, nil, HandlerConfig{
		DefaultDirectory:  t.TempDir(),
		ModelOptions:      modelOpts,
		PermissionOptions: permOpts,
		EffortOptions:     effortOpts,
	}, log.Nop())
	return h, r
}

// defaultPickerHandler uses the same defaults config would supply, so the
// tests assert behaviour against what production sees.
func defaultPickerHandler(t *testing.T) (*Handler, *router.Router) {
	return newPickerTestHandler(t,
		[]string{"haiku", "sonnet", "opus"},
		[]string{"acceptEdits", "plan", "bypassPermissions"},
		[]string{"low", "medium", "high", "xhigh", "max"},
	)
}

// TestPickAnswerValue covers the selection-extraction rule: a custom-typed
// value beats a listed pick; Choices[0] is the fallback for single-select.
func TestPickAnswerValue(t *testing.T) {
	cases := []struct {
		name string
		ans  *protocol.AnswerPayload
		want string
	}{
		{"nil answer", nil, ""},
		{"custom wins over choice", &protocol.AnswerPayload{Custom: "manual-x", Choices: []string{"sonnet"}}, "manual-x"},
		{"choices fallback", &protocol.AnswerPayload{Choices: []string{"sonnet"}}, "sonnet"},
		{"empty everything", &protocol.AnswerPayload{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := pickAnswerValue(c.ans); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestCmdModel_Picker_Success drives the full interactive loop: /model (no
// args) blocks in askAndWait; the test reads the requestID from
// h.answers, delivers the answer, and verifies the router pin.
func TestCmdModel_Picker_Success(t *testing.T) {
	h, r := defaultPickerHandler(t)

	done := make(chan error, 1)
	go func() {
		_, err := h.cmdModel(context.Background(), "chat-1", nil)
		done <- err
	}()

	reqID := waitPending(t, h, time.Second)
	h.deliverAnswer(reqID, &protocol.AnswerPayload{Choices: []string{"sonnet"}})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cmdModel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cmdModel did not return after answer")
	}

	b, _ := r.Lookup("chat-1")
	if b.ModelSpec != "sonnet" {
		t.Errorf("modelSpec = %q, want sonnet", b.ModelSpec)
	}
}

// TestCmdModel_Picker_CustomWins verifies the custom-input box works for
// /model: a typed value overrides the select pick.
func TestCmdModel_Picker_CustomWins(t *testing.T) {
	h, r := defaultPickerHandler(t)

	done := make(chan error, 1)
	go func() {
		_, err := h.cmdModel(context.Background(), "chat-1", nil)
		done <- err
	}()

	reqID := waitPending(t, h, time.Second)
	h.deliverAnswer(reqID, &protocol.AnswerPayload{Choices: []string{"haiku"}, Custom: "claude-sonnet-4-5"})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cmdModel: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cmdModel did not return")
	}

	b, _ := r.Lookup("chat-1")
	if b.ModelSpec != "claude-sonnet-4-5" {
		t.Errorf("modelSpec = %q, want claude-sonnet-4-5 (custom overrides choice)", b.ModelSpec)
	}
}

// TestCmdModel_Clear verifies /model clear removes an existing pin. (A bare
// /model now opens the picker, not a clear.)
func TestCmdModel_Clear(t *testing.T) {
	h, r := defaultPickerHandler(t)
	if _, err := h.cmdModel(context.Background(), "chat-1", []string{"sonnet"}); err != nil {
		t.Fatalf("cmdModel set: %v", err)
	}
	if _, err := h.cmdModel(context.Background(), "chat-1", []string{"clear"}); err != nil {
		t.Fatalf("cmdModel clear: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.ModelSpec != "" {
		t.Errorf("modelSpec = %q, want empty after clear", b.ModelSpec)
	}
}

// TestCmdEffort_Picker_Success is the effort analogue. No custom input: the
// picker restricts to the listed levels.
func TestCmdEffort_Picker_Success(t *testing.T) {
	h, r := defaultPickerHandler(t)

	done := make(chan error, 1)
	go func() {
		_, err := h.cmdEffort(context.Background(), "chat-1", nil)
		done <- err
	}()

	reqID := waitPending(t, h, time.Second)
	h.deliverAnswer(reqID, &protocol.AnswerPayload{Choices: []string{"max"}})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cmdEffort: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cmdEffort did not return")
	}

	b, _ := r.Lookup("chat-1")
	if b.EffortLevel != "max" {
		t.Errorf("effortLevel = %q, want max", b.EffortLevel)
	}
}

// TestCmdEffort_Clear verifies /effort clear.
func TestCmdEffort_Clear(t *testing.T) {
	h, r := defaultPickerHandler(t)
	if _, err := h.cmdEffort(context.Background(), "chat-1", []string{"max"}); err != nil {
		t.Fatalf("cmdEffort set: %v", err)
	}
	if _, err := h.cmdEffort(context.Background(), "chat-1", []string{"clear"}); err != nil {
		t.Fatalf("cmdEffort clear: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.EffortLevel != "" {
		t.Errorf("effortLevel = %q, want empty after clear", b.EffortLevel)
	}
}

// TestCmdPerm_Picker_Success is the permission analogue.
func TestCmdPerm_Picker_Success(t *testing.T) {
	h, r := defaultPickerHandler(t)

	done := make(chan error, 1)
	go func() {
		_, err := h.cmdPermission(context.Background(), "chat-1", nil)
		done <- err
	}()

	reqID := waitPending(t, h, time.Second)
	h.deliverAnswer(reqID, &protocol.AnswerPayload{Choices: []string{"plan"}})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cmdPermission: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cmdPermission did not return")
	}

	b, _ := r.Lookup("chat-1")
	if b.PermissionMode != "plan" {
		t.Errorf("permissionMode = %q, want plan", b.PermissionMode)
	}
}

// TestCmdPerm_Clear verifies /perm clear falls back to the default.
func TestCmdPerm_Clear(t *testing.T) {
	h, r := defaultPickerHandler(t)
	if _, err := h.cmdPermission(context.Background(), "chat-1", []string{"plan"}); err != nil {
		t.Fatalf("cmdPermission set: %v", err)
	}
	if _, err := h.cmdPermission(context.Background(), "chat-1", []string{"clear"}); err != nil {
		t.Fatalf("cmdPermission clear: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.PermissionMode != "" {
		t.Errorf("permissionMode = %q, want empty after clear", b.PermissionMode)
	}
}

// TestCmdModel_Picker_EmptyAnswer verifies an answer with no choice and no
// custom text is rejected, leaving the binding untouched. The picker returns
// Handled (its error surfaces via an error Notice, not via the dispatch error
// path), so the assertion is on the router state, not a returned error.
func TestCmdModel_Picker_EmptyAnswer(t *testing.T) {
	h, r := defaultPickerHandler(t)

	done := make(chan commandResult, 1)
	go func() {
		res, _ := h.cmdModel(context.Background(), "chat-1", nil)
		done <- res
	}()

	reqID := waitPending(t, h, time.Second)
	h.deliverAnswer(reqID, &protocol.AnswerPayload{})

	select {
	case res := <-done:
		if !res.Handled {
			t.Error("picker should always return Handled=true")
		}
		if !strings.Contains(res.Body, "未选择") {
			t.Errorf("Body should mention no selection, got: %q", res.Body)
		}
	case <-time.After(time.Second):
		t.Fatal("cmdModel did not return")
	}
	b, _ := r.Lookup("chat-1")
	if b.ModelSpec != "" {
		t.Errorf("modelSpec = %q, want empty after empty answer", b.ModelSpec)
	}
}

// --- helpers ---

// waitPending polls h.answers until a slot appears, returning its
// requestID. Fails the test if no slot appears within timeout.
func waitPending(t *testing.T, h *Handler, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ids := h.answers.PendingIDs(); len(ids) > 0 {
			return ids[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no pending answer slot appeared within timeout")
	return ""
}

// settingsFakeAgent is a claudeAPI fake whose ListSettings returns the given
// paths (full paths, as the real Client would). Run is unused by the
// settings picker path.
type settingsFakeAgent struct {
	paths []string
}

func (settingsFakeAgent) Run(context.Context, claude.RunOptions) (<-chan claude.Event, error) {
	ch := make(chan claude.Event)
	close(ch)
	return ch, nil
}

func (f settingsFakeAgent) ListSettings(context.Context) ([]string, error) {
	return f.paths, nil
}

// newSettingsPickerHandler wires a Handler with a settingsFakeAgent so the
// /settings picker can be exercised end to end.
func newSettingsPickerHandler(t *testing.T, paths []string) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New(nil, "", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, settingsFakeAgent{paths: paths}, nil, HandlerConfig{
		DefaultDirectory: t.TempDir(),
	}, log.Nop())
	return h, r
}

// TestCmdSettings_Picker_Success drives the settings picker: /settings (no
// args) → agent lists paths → card shows basenames → user picks → full path
// pinned on the binding.
func TestCmdSettings_Picker_Success(t *testing.T) {
	dir := "/home/user/.claude"
	paths := []string{
		filepath.Join(dir, "kimi-settings.json"),
		filepath.Join(dir, "settings.json"),
		filepath.Join(dir, "zhipu-settings.json"),
	}
	h, r := newSettingsPickerHandler(t, paths)

	done := make(chan error, 1)
	go func() {
		_, err := h.cmdSettings(context.Background(), "chat-1", nil)
		done <- err
	}()

	reqID := waitPending(t, h, time.Second)
	// User selects the "kimi-settings.json" option (a basename).
	h.deliverAnswer(reqID, &protocol.AnswerPayload{Choices: []string{"kimi-settings.json"}})

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cmdSettings: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cmdSettings did not return")
	}

	b, _ := r.Lookup("chat-1")
	want := filepath.Join(dir, "kimi-settings.json")
	if b.SettingsFile != want {
		t.Errorf("settingsFile = %q, want %q (basename must resolve to full path)", b.SettingsFile, want)
	}
}

// TestCmdSettings_Clear verifies /settings clear removes an existing pin.
func TestCmdSettings_Clear(t *testing.T) {
	h, r := defaultPickerHandler(t)
	// The picker is the only way to pin a settings file now; emulate a prior
	// pick by seeding the binding directly, then clear it.
	if _, err := h.ensureBinding("chat-1", "", "", "", ""); err != nil {
		t.Fatalf("ensureBinding: %v", err)
	}
	h.router.SetSettingsFile("chat-1", "/etc/claude/k.json")

	if _, err := h.cmdSettings(context.Background(), "chat-1", []string{"clear"}); err != nil {
		t.Fatalf("cmdSettings clear: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.SettingsFile != "" {
		t.Errorf("settingsFile = %q, want empty after clear", b.SettingsFile)
	}
}

// TestCmdSettings_Picker_EmptyList verifies the picker surfaces a warning when
// the settings directory has no matching files.
func TestCmdSettings_Picker_EmptyList(t *testing.T) {
	h, _ := newSettingsPickerHandler(t, nil)

	res, _ := h.cmdSettings(context.Background(), "chat-1", nil)
	if !res.Handled {
		t.Error("empty-list picker should return Handled=true")
	}
	if !strings.Contains(res.Body, "没有") {
		t.Errorf("Body should mention no settings files, got: %q", res.Body)
	}
}
