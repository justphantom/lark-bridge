package peribridge

import (
	"context"
	"strings"
	"testing"

	"github.com/hu/lark-bridge/internal/router"
)

// TestSettablePermissionModes verifies the /perm picker's value gate: the
// three non-interactive modes are accepted, "default" (deadlocks -p) is not.
func TestSettablePermissionModes(t *testing.T) {
	for _, m := range []string{"bypass", "accept-edit", "auto-mode"} {
		if !isSettablePermissionMode(m) {
			t.Errorf("%q should be settable", m)
		}
	}
	if isSettablePermissionMode("default") {
		t.Error(`"default" must NOT be settable (deadlocks -p subprocess)`)
	}
	if isSettablePermissionMode("nonsense") {
		t.Error(`"nonsense" must not be settable`)
	}
}

// TestSettableEffortLevels verifies the /effort picker's value gate matches
// peri's documented low/medium/high/max.
func TestSettableEffortLevels(t *testing.T) {
	for _, l := range []string{"low", "medium", "high", "max"} {
		if !isSettableEffortLevel(l) {
			t.Errorf("%q should be settable", l)
		}
	}
	for _, l := range []string{"xhigh", "ultra", ""} {
		if isSettableEffortLevel(l) {
			t.Errorf("%q must not be settable", l)
		}
	}
}

// TestDefaultOptions verifies the picker fallbacks used when config omits the
// option lists.
func TestDefaultOptions(t *testing.T) {
	if len(defaultPermissionOptions) != 3 {
		t.Errorf("defaultPermissionOptions len = %d, want 3", len(defaultPermissionOptions))
	}
	if len(defaultEffortOptions) != 4 {
		t.Errorf("defaultEffortOptions len = %d, want 4", len(defaultEffortOptions))
	}
}

// TestFormatBindings verifies the /session-list renderer: empty map, title
// sort, and the current-chat marker.
func TestFormatBindings(t *testing.T) {
	if got := formatBindings(nil, "x"); got != "暂无绑定。" {
		t.Errorf("empty = %q", got)
	}
	bindings := map[string]router.Binding{
		"chat-a": {Title: "Alpha", ModelSpec: "sonnet"},
		"chat-b": {Title: "Beta", ModelSpec: ""},
	}
	got := formatBindings(bindings, "chat-b")
	if !strings.Contains(got, "Alpha [sonnet]") {
		t.Errorf("missing Alpha row: %q", got)
	}
	if !strings.Contains(got, "Beta [默认]") {
		t.Errorf("missing Beta default model: %q", got)
	}
	if !strings.Contains(got, "← 当前") {
		t.Errorf("missing current marker: %q", got)
	}
	// Alpha must precede Beta (title sort).
	if i, j := strings.Index(got, "Alpha"), strings.Index(got, "Beta"); i > j {
		t.Errorf("not sorted by title: %q", got)
	}
}

// TestValidateSettingsPath covers the traversal guard for --settings paths.
func TestValidateSettingsPath(t *testing.T) {
	for _, p := range []string{"", "s.json", "/abs/s.json", "~/peri/s.json"} {
		if err := validateSettingsPath(p); err != nil {
			t.Errorf("validateSettingsPath(%q) = %v, want nil", p, err)
		}
	}
	for _, p := range []string{"../escape", "../../etc/passwd"} {
		if err := validateSettingsPath(p); err == nil {
			t.Errorf("validateSettingsPath(%q) = nil, want error", p)
		}
	}
}

// TestCmdPermission_DirectPin verifies the /perm handler pins a valid mode and
// rejects an invalid one. Uses a real router + nil rpc (emit is a no-op).
func TestCmdPermission_DirectPin(t *testing.T) {
	h, _, cleanup := newTestHandler(t, closedStreamPeri{})
	defer cleanup()
	h.Router.Bind("chat-p", "", t.TempDir(), "T", "", "")

	// Valid pin.
	res, err := h.cmdPermission(context.Background(), "chat-p", []string{"accept-edit"})
	if err != nil {
		t.Fatalf("cmdPermission valid: %v", err)
	}
	if res.After != "accept-edit" {
		t.Errorf("result After = %q, want accept-edit", res.After)
	}
	b, _ := h.Router.Lookup("chat-p")
	if b.PermissionMode != "accept-edit" {
		t.Errorf("binding perm = %q, want accept-edit", b.PermissionMode)
	}

	// Invalid pin → error result, binding unchanged.
	res, err = h.cmdPermission(context.Background(), "chat-p", []string{"default"})
	if err == nil {
		t.Fatal("expected error for default mode")
	}
	b, _ = h.Router.Lookup("chat-p")
	if b.PermissionMode != "accept-edit" {
		t.Errorf("binding perm changed on reject: %q", b.PermissionMode)
	}
}

// TestCmdEffort_DirectPin verifies the /effort handler pins a valid level and
// rejects an invalid one.
func TestCmdEffort_DirectPin(t *testing.T) {
	h, _, cleanup := newTestHandler(t, closedStreamPeri{})
	defer cleanup()
	h.Router.Bind("chat-e", "", t.TempDir(), "T", "", "")

	res, err := h.cmdEffort(context.Background(), "chat-e", []string{"max"})
	if err != nil {
		t.Fatalf("cmdEffort valid: %v", err)
	}
	if res.After != "max" {
		t.Errorf("result After = %q, want max", res.After)
	}
	b, _ := h.Router.Lookup("chat-e")
	if b.EffortLevel != "max" {
		t.Errorf("binding effort = %q, want max", b.EffortLevel)
	}

	// Invalid level.
	_, err = h.cmdEffort(context.Background(), "chat-e", []string{"ultra"})
	if err == nil {
		t.Fatal("expected error for ultra level")
	}
}

// TestCmdCurrent_ShowAllSettings verifies /current now lists perm/effort/settings.
func TestCmdCurrent_ShowAllSettings(t *testing.T) {
	h, _, cleanup := newTestHandler(t, closedStreamPeri{})
	defer cleanup()
	dir := t.TempDir()
	h.Router.Bind("chat-c", "", dir, "T", "sonnet", "")
	h.Router.SetPermissionMode("chat-c", "accept-edit")
	h.Router.SetEffortLevel("chat-c", "high")
	h.Router.SetSettingsFile("chat-c", "/tmp/s.json")

	res, err := h.cmdCurrent(context.Background(), "chat-c", nil)
	if err != nil {
		t.Fatalf("cmdCurrent: %v", err)
	}
	for _, want := range []string{"accept-edit", "high", "/tmp/s.json", "sonnet", dir} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("current body missing %q:\n%s", want, res.Body)
		}
	}
}

// TestCmdListSessions verifies /session-list renders all bindings.
func TestCmdListSessions(t *testing.T) {
	h, _, cleanup := newTestHandler(t, closedStreamPeri{})
	defer cleanup()
	h.Router.Bind("a", "", t.TempDir(), "Alpha", "", "")
	h.Router.Bind("b", "", t.TempDir(), "Beta", "sonnet", "")

	res, err := h.cmdListSessions(context.Background(), "a", nil)
	if err != nil {
		t.Fatalf("cmdListSessions: %v", err)
	}
	if !strings.Contains(res.Body, "Alpha") || !strings.Contains(res.Body, "Beta") {
		t.Errorf("list missing rows:\n%s", res.Body)
	}
	if !strings.Contains(res.Body, "← 当前") {
		t.Errorf("list missing current marker:\n%s", res.Body)
	}
}
