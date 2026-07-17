package claudebridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/cmdutil"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/router"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in       string
		wantCmd  string
		wantArgs []string
	}{
		{"/model claude-sonnet-4-5", "/model", []string{"claude-sonnet-4-5"}},
		{"/effort max", "/effort", []string{"max"}},
		{"/perm plan", "/perm", []string{"plan"}},
		{"/cd /home/user/repo", "/cd", []string{"/home/user/repo"}},
		{"/help", "/help", []string{}},
		{"hello world", "", nil},
		{"   /current   ", "/current", []string{}},
		{"/session-new", "/session-new", []string{}},
	}
	for _, tc := range tests {
		gotCmd, gotArgs := cmdutil.ParseCommand(tc.in)
		if gotCmd != tc.wantCmd {
			t.Errorf("ParseCommand(%q) cmd = %q, want %q", tc.in, gotCmd, tc.wantCmd)
		}
		if len(gotArgs) != len(tc.wantArgs) {
			t.Errorf("ParseCommand(%q) args = %v, want %v", tc.in, gotArgs, tc.wantArgs)
			continue
		}
		for i := range gotArgs {
			if gotArgs[i] != tc.wantArgs[i] {
				t.Errorf("ParseCommand(%q) args[%d] = %q, want %q", tc.in, i, gotArgs[i], tc.wantArgs[i])
			}
		}
	}
}

func TestSettablePermissionModes(t *testing.T) {
	for _, m := range []string{"acceptEdits", "plan", "bypassPermissions"} {
		if !isSettablePermissionMode(m) {
			t.Errorf("%q should be settable", m)
		}
	}
	// "default" is rejected: it prompts interactively and would deadlock the
	// non-interactive -p subprocess.
	if isSettablePermissionMode("default") {
		t.Errorf("default should NOT be settable")
	}
}

func TestSettableEffortLevels(t *testing.T) {
	for _, l := range []string{"low", "medium", "high", "xhigh", "max"} {
		if !isSettableEffortLevel(l) {
			t.Errorf("%q should be settable", l)
		}
	}
	if isSettableEffortLevel("ultra") {
		t.Errorf("ultra should NOT be settable")
	}
}

// newCmdTestHandler builds a Handler with a fresh in-memory router and no
// rpc/agent (commands tested here do not run or emit). Suitable for the
// lazy-bind command tests.
func newCmdTestHandler(t *testing.T) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New(nil, "", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, nil, nil, HandlerConfig{DefaultDirectory: t.TempDir()}, log.Nop())
	return h, r
}

// TestCmdModel_LazyBind verifies a config command issued before any
// conversation lazily creates the binding so the pin lands before the first
// prompt.
func TestCmdModel_LazyBind(t *testing.T) {
	h, r := newCmdTestHandler(t)

	res, err := h.cmdModel(context.Background(), "chat-1", []string{"claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("cmdModel: %v", err)
	}
	b, ok := r.Lookup("chat-1")
	if !ok {
		t.Fatal("expected lazy binding creation before first prompt")
	}
	if b.ModelSpec != "claude-sonnet-4-5" {
		t.Errorf("modelSpec = %q, want claude-sonnet-4-5", b.ModelSpec)
	}
	if res.Field != "模型" || res.After != "claude-sonnet-4-5" {
		t.Errorf("change result = {Field:%q After:%q}, want 模型/claude-sonnet-4-5", res.Field, res.After)
	}
}

// TestCmdDirectory_LazyBind verifies /cd also creates the binding lazily and
// pins the requested directory (which must be under WORKSPACE_ROOT), and
// returns a structured change result.
func TestCmdDirectory_LazyBind(t *testing.T) {
	h, r := newCmdTestHandler(t)
	workspace := t.TempDir()
	dir := filepath.Join(workspace, "proj-1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h.DirCache = bridgebase.NewDirCache(workspace)

	res, err := h.cmdDirectory(context.Background(), "chat-1", []string{dir})
	if err != nil {
		t.Fatalf("cmdDirectory: %v", err)
	}
	b, ok := r.Lookup("chat-1")
	if !ok {
		t.Fatal("expected lazy binding creation before first prompt")
	}
	if b.Directory != dir {
		t.Errorf("directory = %q, want %q", b.Directory, dir)
	}
	if res.Field != "工作目录" || res.After != dir {
		t.Errorf("change result = {Field:%q After:%q}, want 工作目录/%s", res.Field, res.After, dir)
	}
}

// TestCmdEffort_RejectsUnknown verifies /effort rejects a value outside the
// allowed set.
func TestCmdEffort_RejectsUnknown(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	_, err := h.cmdEffort(context.Background(), "chat-1", []string{"ultra"})
	if err == nil {
		t.Fatal("expected error for unknown effort level, got nil")
	}
}

// TestCmdEffort_Pins verifies /effort with a valid level pins it on the
// binding and returns a structured change result.
func TestCmdEffort_Pins(t *testing.T) {
	h, r := newCmdTestHandler(t)
	res, err := h.cmdEffort(context.Background(), "chat-1", []string{"max"})
	if err != nil {
		t.Fatalf("cmdEffort: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.EffortLevel != "max" {
		t.Errorf("effortLevel = %q, want max", b.EffortLevel)
	}
	if res.Field != "推理级别" || res.After != "max" {
		t.Errorf("change result = {Field:%q After:%q}, want 推理级别/max", res.Field, res.After)
	}
}

// TestCmdSettings_RejectsCustomPath verifies /settings <path> is no longer
// accepted: the file must come from the picker list, not a free-form argument.
// Any non-"clear" argument is rejected and nothing is pinned.
func TestCmdSettings_RejectsCustomPath(t *testing.T) {
	h, r := newCmdTestHandler(t)

	for _, path := range []string{"/etc/claude/kimi.json", "../../etc/passwd"} {
		_, err := h.cmdSettings(context.Background(), "chat-1", []string{path})
		if err == nil {
			t.Errorf("cmdSettings(%q): expected error, got nil", path)
		}
	}
	b, _ := r.Lookup("chat-1")
	if b.SettingsFile != "" {
		t.Errorf("SettingsFile = %q, want empty (custom path must not pin)", b.SettingsFile)
	}
}

// TestCmdPermission_RejectsDefault verifies /perm rejects "default".
func TestCmdPermission_RejectsDefault(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	_, err := h.cmdPermission(context.Background(), "chat-1", []string{"default"})
	if err == nil {
		t.Fatal("expected error for default permission mode, got nil")
	}
}

// TestCmdPermission_Pins verifies /perm with a valid mode pins it on the
// binding and returns a structured change result (field/before/after) so the
// notice card can render the before→after block.
func TestCmdPermission_Pins(t *testing.T) {
	h, r := newCmdTestHandler(t)
	res, err := h.cmdPermission(context.Background(), "chat-1", []string{"plan"})
	if err != nil {
		t.Fatalf("cmdPermission: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.PermissionMode != "plan" {
		t.Errorf("permissionMode = %q, want plan", b.PermissionMode)
	}
	if res.Field != "权限模式" || res.After != "plan" {
		t.Errorf("change result = {Field:%q After:%q}, want 权限模式/plan", res.Field, res.After)
	}
	if res.Before == "" {
		t.Error("before should be non-empty (default fallback) on first set")
	}
}

// TestCmdSessionNew_NoBinding verifies /session-new on a chat with no binding
// reports the no-session message instead of creating one.
func TestCmdSessionNew_NoBinding(t *testing.T) {
	h, r := newCmdTestHandler(t)
	res, err := h.cmdSessionNew(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionNew: %v", err)
	}
	if _, ok := r.Lookup("chat-1"); ok {
		t.Error("expected no binding to be created on /session-new with no prior binding")
	}
	if res.Body == "" {
		t.Error("expected non-empty body explaining there is no session")
	}
}

// TestCmdSessionDel_NoBinding verifies /session-del on a chat with no binding
// reports the no-binding message instead of erroring.
func TestCmdSessionDel_NoBinding(t *testing.T) {
	h, r := newCmdTestHandler(t)
	res, err := h.cmdSessionDel(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionDel: %v", err)
	}
	if _, ok := r.Lookup("chat-1"); ok {
		t.Error("expected no binding to be created on /session-del with no prior binding")
	}
	if res.Body == "" {
		t.Error("expected non-empty body explaining there is no binding")
	}
}

// TestCmdSessionDel_RemovesBinding verifies /session-del removes an existing
// binding entirely.
func TestCmdSessionDel_RemovesBinding(t *testing.T) {
	h, r := newCmdTestHandler(t)
	// Create a binding via /cd so a directory is allocated. The dir must be
	// under workspaceRoot to pass the new validation.
	workspace := t.TempDir()
	dir := filepath.Join(workspace, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h.DirCache = bridgebase.NewDirCache(workspace)
	if _, err := h.cmdDirectory(context.Background(), "chat-1", []string{dir}); err != nil {
		t.Fatalf("cmdDirectory: %v", err)
	}
	if _, err := h.cmdSessionDel(context.Background(), "chat-1", nil); err != nil {
		t.Fatalf("cmdSessionDel: %v", err)
	}
	if _, ok := r.Lookup("chat-1"); ok {
		t.Error("expected binding to be removed after /session-del")
	}
}

// TestCmdSessionAbort_Idle verifies /session-abort with nothing in flight
// reports that there is no running call (returns no error).
func TestCmdSessionAbort_Idle(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	_, err := h.cmdSessionAbort(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdSessionAbort idle: %v", err)
	}
}

// TestValidateWorkspacePath covers the workspace containment guard.
func TestValidateWorkspacePath(t *testing.T) {
	root := "/home/user/projects"
	cases := []struct {
		name    string
		dir     string
		root    string
		wantErr bool
	}{
		{"empty root rejects all", "/home/user/projects/a", "", true},
		{"direct child ok", "/home/user/projects/a", root, false},
		{"nested child ok", "/home/user/projects/a/b/c", root, false},
		{"sibling outside root rejected", "/home/user/other", root, true},
		{"parent traversal rejected", "/home/user/projects/../../etc", root, true},
		{"unrelated absolute rejected", "/etc/passwd", root, true},
		{"root itself ok (rel=.)", root, root, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := bridgebase.NewDirCache(c.root).Validate(c.dir)
			if c.wantErr && err == nil {
				t.Errorf("validateWorkspacePath(%q,%q) expected error", c.dir, c.root)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validateWorkspacePath(%q,%q) unexpected error: %v", c.dir, c.root, err)
			}
		})
	}
}

// TestCmdDirectory_RejectsEscape verifies /cd with a path outside workspace
// is rejected — the binding keeps its default directory (from ensureBinding),
// not the rejected path.
func TestCmdDirectory_RejectsEscape(t *testing.T) {
	h, r := newCmdTestHandler(t)
	h.DirCache = bridgebase.NewDirCache(t.TempDir())
	_, err := h.cmdDirectory(context.Background(), "chat-1", []string{"/etc"})
	if err == nil {
		t.Fatal("expected error for path outside workspace")
	}
	b, _ := r.Lookup("chat-1")
	if b.Directory == "/etc" {
		t.Errorf("Directory = /etc; rejected path must not pin")
	}
}

// TestCmdDirectory_Clear verifies /cd clear resets the directory pin.
func TestCmdDirectory_Clear(t *testing.T) {
	h, r := newCmdTestHandler(t)
	workspace := t.TempDir()
	dir := filepath.Join(workspace, "proj")
	os.MkdirAll(dir, 0o755)
	h.DirCache = bridgebase.NewDirCache(workspace)
	// Pin first.
	if _, err := h.cmdDirectory(context.Background(), "chat-1", []string{dir}); err != nil {
		t.Fatalf("cmdDirectory set: %v", err)
	}
	// Clear.
	if _, err := h.cmdDirectory(context.Background(), "chat-1", []string{"clear"}); err != nil {
		t.Fatalf("cmdDirectory clear: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.Directory != "" {
		t.Errorf("Directory = %q, want empty after clear", b.Directory)
	}
}

// TestListWorkspaceDirs scans immediate subdirectories and caches.
func TestListWorkspaceDirs(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	workspace := t.TempDir()
	os.MkdirAll(filepath.Join(workspace, "proj-a"), 0o755)
	os.MkdirAll(filepath.Join(workspace, "proj-b"), 0o755)
	os.WriteFile(filepath.Join(workspace, "not-a-dir.txt"), []byte("x"), 0o644) // skipped
	h.DirCache = bridgebase.NewDirCache(workspace)

	dirs, err := h.DirCache.List()
	if err != nil {
		t.Fatalf("listWorkspaceDirs: %v", err)
	}
	if len(dirs) != 2 {
		t.Fatalf("got %d dirs, want 2: %v", len(dirs), dirs)
	}
	if filepath.Base(dirs[0]) != "proj-a" || filepath.Base(dirs[1]) != "proj-b" {
		t.Errorf("dirs not sorted or wrong: %v", dirs)
	}
}

// TestListWorkspaceDirs_EmptyRoot verifies an unset workspaceRoot errors.
func TestListWorkspaceDirs_EmptyRoot(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	_, err := h.DirCache.List()
	if err == nil {
		t.Fatal("expected error for empty workspaceRoot")
	}
}

// TestValidateSettingsPath covers the traversal guard: empty and normal paths
// pass; paths that Clean to an upward escape are rejected.
func TestValidateSettingsPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"empty allowed (clear)", "", false},
		{"absolute normal", "/home/u/.claude/k.json", false},
		{"absolute with .. that cleans down", "/a/../b.json", false},
		{"relative normal", "k.json", false},
		{"relative subpath", "sub/k.json", false},
		{"relative ..", "..", true},
		{"relative ../", "../etc/passwd", true},
		{"deep ../", "../../etc/shadow", true},
		{"leading dotdot slash", "../../../root", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateSettingsPath(c.path)
			if c.wantErr && err == nil {
				t.Errorf("validateSettingsPath(%q) expected error, got nil", c.path)
			}
			if !c.wantErr && err != nil {
				t.Errorf("validateSettingsPath(%q) unexpected error: %v", c.path, err)
			}
		})
	}
}
