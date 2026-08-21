package miniagent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// fakeCommander records the last Run invocation without shelling out.
type fakeCommander struct {
	mu     sync.Mutex
	called bool
	dir    string
	name   string
	args   []string
}

func (f *fakeCommander) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.dir = dir
	f.name = name
	f.args = append([]string{}, args...)
	return []byte("ok"), nil
}

func (f *fakeCommander) snapshot() (called bool, dir, name string, args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called, f.dir, f.name, append([]string{}, f.args...)
}

func newMiniTaskHandler(t *testing.T, cmd *fakeCommander, workspaceRoot string) *Handler {
	t.Helper()
	h := New(&captureSender{}, log.Nop(), nil, workspaceRoot, "test-model", "", nil, "", 0, "", false)
	h.runner = NewTaskRunner(cmd, nil, 0)
	return h
}

func waitForTaskCalled(t *testing.T, c *fakeCommander) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if called, _, _, _ := c.snapshot(); called {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("commander not called within timeout")
}

// TestCmdPull_NoWorkspace verifies /pull surfaces a warning when no
// workspace_root is configured (the operator never set WORKSPACE_ROOT).
func TestCmdPull_NoWorkspace(t *testing.T) {
	cmd := &fakeCommander{}
	h := newMiniTaskHandler(t, cmd, "")

	level, _, body := h.cmdPull(context.Background(), "chat-empty", "")
	if level != "warning" {
		t.Errorf("level = %q, want warning", level)
	}
	if !strings.Contains(body, "尚未配置工作目录") {
		t.Errorf("body = %q, want config hint", body)
	}
	if called, _, _, _ := cmd.snapshot(); called {
		t.Error("git must not be invoked without a workspace")
	}
}

// TestCmdPull_WithWorkspace verifies /pull triggers git in the
// workspace_root fallback directory and returns the async sentinel so the
// dispatcher does not double-emit a notice.
func TestCmdPull_WithWorkspace(t *testing.T) {
	cmd := &fakeCommander{}
	h := newMiniTaskHandler(t, cmd, "/repo/ws")

	level, _, _ := h.cmdPull(context.Background(), "chat-1", "")
	if level != "async" {
		t.Errorf("level = %q, want async sentinel", level)
	}
	waitForTaskCalled(t, cmd)

	called, dir, name, args := cmd.snapshot()
	if !called {
		t.Fatal("git not invoked")
	}
	if dir != "/repo/ws" {
		t.Errorf("git dir = %q, want /repo/ws", dir)
	}
	if name != "git" || len(args) != 2 || args[0] != "pull" || args[1] != "--ff-only" {
		t.Errorf("git args = %v %v, want [git pull --ff-only]", name, args)
	}
}

// TestCmdPush_WithWorkspace verifies /push forwards `git push`.
func TestCmdPush_WithWorkspace(t *testing.T) {
	cmd := &fakeCommander{}
	h := newMiniTaskHandler(t, cmd, "/repo/ws")

	h.cmdPush(context.Background(), "chat-1", "")
	waitForTaskCalled(t, cmd)

	_, _, name, args := cmd.snapshot()
	if name != "git" || len(args) != 1 || args[0] != "push" {
		t.Errorf("cmd = %s %v, want [git push]", name, args)
	}
}

// TestCmdBuild_WithWorkspace verifies /build forwards `make build`.
func TestCmdBuild_WithWorkspace(t *testing.T) {
	cmd := &fakeCommander{}
	h := newMiniTaskHandler(t, cmd, "/repo/ws")

	h.cmdBuild(context.Background(), "chat-1", "")
	waitForTaskCalled(t, cmd)

	_, _, name, args := cmd.snapshot()
	if name != "make" || len(args) != 1 || args[0] != "build" {
		t.Errorf("cmd = %s %v, want [make build]", name, args)
	}
}
