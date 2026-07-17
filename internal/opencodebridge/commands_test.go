package opencodebridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		{"/model glm-5-turbo", "/model", []string{"glm-5-turbo"}},
		{"/agent build", "/agent", []string{"build"}},
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

	res, err := h.cmdModel(context.Background(), "chat-1", []string{"glm-5-turbo"})
	if err != nil {
		t.Fatalf("cmdModel: %v", err)
	}
	b, ok := r.Lookup("chat-1")
	if !ok {
		t.Fatal("expected lazy binding creation before first prompt")
	}
	if b.ModelSpec != "glm-5-turbo" {
		t.Errorf("modelSpec = %q, want glm-5-turbo", b.ModelSpec)
	}
	if res.Field != "模型" || res.After != "glm-5-turbo" {
		t.Errorf("change result = {Field:%q After:%q}, want 模型/glm-5-turbo", res.Field, res.After)
	}
}

// TestCmdAgent_LazyBind verifies /agent also creates the binding lazily and
// pins the requested agent, and returns a structured change result.
func TestCmdAgent_LazyBind(t *testing.T) {
	h, r := newCmdTestHandler(t)

	res, err := h.cmdAgent(context.Background(), "chat-1", []string{"build"})
	if err != nil {
		t.Fatalf("cmdAgent: %v", err)
	}
	b, ok := r.Lookup("chat-1")
	if !ok {
		t.Fatal("expected lazy binding creation before first prompt")
	}
	if b.Agent != "build" {
		t.Errorf("agent = %q, want build", b.Agent)
	}
	if res.Field != "agent" || res.After != "build" {
		t.Errorf("change result = {Field:%q After:%q}, want agent/build", res.Field, res.After)
	}
}

// TestCmdDirectory_LazyBind verifies /cd also creates the binding lazily and
// pins the requested directory, and returns a structured change result.
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

// TestCmdModel_Clear verifies /model clear removes an existing pin. (A bare
// /model now opens the interactive picker, not a clear.)
func TestCmdModel_Clear(t *testing.T) {
	h, r := newCmdTestHandler(t)
	if _, err := h.cmdModel(context.Background(), "chat-1", []string{"glm-5-turbo"}); err != nil {
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

// TestCmdDirectory_RejectsRelative verifies /cd rejects a relative path with
// an error. ensureBinding still runs first and creates a binding; this test
// only asserts the validation error path.
func TestCmdDirectory_RejectsRelative(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	// Set workspaceRoot so the rejection comes from validateAbsDir (relative
	// path) rather than validateWorkspacePath (workspace unconfigured).
	h.DirCache = bridgebase.NewDirCache(t.TempDir())
	_, err := h.cmdDirectory(context.Background(), "chat-1", []string{"relative/path"})
	if err == nil {
		t.Fatal("expected error for relative path, got nil")
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
// is rejected — the binding keeps its default directory, not the rejected
// path.
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
	if _, err := h.cmdDirectory(context.Background(), "chat-1", []string{dir}); err != nil {
		t.Fatalf("cmdDirectory set: %v", err)
	}
	if _, err := h.cmdDirectory(context.Background(), "chat-1", []string{"clear"}); err != nil {
		t.Fatalf("cmdDirectory clear: %v", err)
	}
	b, _ := r.Lookup("chat-1")
	if b.Directory != "" {
		t.Errorf("Directory = %q, want empty after clear", b.Directory)
	}
}

// TestListWorkspaceDirs scans immediate subdirectories.
func TestListWorkspaceDirs(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	workspace := t.TempDir()
	os.MkdirAll(filepath.Join(workspace, "proj-a"), 0o755)
	os.MkdirAll(filepath.Join(workspace, "proj-b"), 0o755)
	os.WriteFile(filepath.Join(workspace, "not-a-dir.txt"), []byte("x"), 0o644)
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

// TestCmdRunning_Empty verifies /running with no in-flight prompts reports
// the empty state.
func TestCmdRunning_Empty(t *testing.T) {
	h, _ := newCmdTestHandler(t)
	res, err := h.cmdRunning(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("cmdRunning: %v", err)
	}
	if !strings.Contains(res.Body, "没有运行中的会话") {
		t.Errorf("Body = %q, want contains '没有运行中的会话'", res.Body)
	}
}

// TestCmdRunning_ListsSessionsWithAgent verifies /running lists an in-flight
// prompt with its title, model, and agent. We inject a promptCancel directly
// into cancelByChat (same-package test) and seed the router binding so
// TitleOf/Lookup resolve.
func TestCmdRunning_ListsSessionsWithAgent(t *testing.T) {
	h, r := newCmdTestHandler(t)
	// Seed a binding with model + agent so the running card shows them.
	r.Bind("chat-1", "sess-1", "", "测试群", "glm-5.2", "build")
	// Inject an in-flight prompt.
	h.CancelMu.Lock()
	h.CancelByChat["chat-1"] = &bridgebase.PromptCancel{
		Cancel:    func() {},
		StartTime: time.Now().Add(-30 * time.Second),
		ChatID:    "chat-1",
	}
	h.CancelMu.Unlock()

	res, err := h.cmdRunning(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("cmdRunning: %v", err)
	}
	for _, want := range []string{"运行中会话", "测试群", "glm-5.2", "build", "30秒"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("Body missing %q\ngot:\n%s", want, res.Body)
		}
	}
}
