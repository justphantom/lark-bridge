package opencodeservebridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// gitFakeCommander records the last Run invocation without shelling out.
// Fields are guarded by mu because the GitRunner calls Run on its own
// goroutine while the test reads them.
type gitFakeCommander struct {
	mu     sync.Mutex
	called bool
	dir    string
	name   string
	args   []string
	out    []byte
	err    error
}

func (f *gitFakeCommander) Run(_ context.Context, dir, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.called = true
	f.dir = dir
	f.name = name
	f.args = append([]string{}, args...)
	out, err := f.out, f.err
	f.mu.Unlock()
	return out, err
}

func (f *gitFakeCommander) snapshot() (called bool, dir, name string, args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.called, f.dir, f.name, append([]string{}, f.args...)
}

func waitForCalled(t *testing.T, c *gitFakeCommander) {
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

func newGitTestHandler(t *testing.T, cmd *gitFakeCommander) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, nil, nil, HandlerConfig{DefaultDirectory: t.TempDir()}, log.Nop())
	h.Git = bridgebase.NewGitRunner(cmd, nil, 0)
	return h, r
}

// TestCmdPull_NoDirectory verifies /pull before /cd surfaces a usage hint
// instead of forking git in the process CWD.
func TestCmdPull_NoDirectory(t *testing.T) {
	cmd := &gitFakeCommander{}
	h, _ := newGitTestHandler(t, cmd)

	res, err := h.cmdPull(context.Background(), "chat-empty", nil)
	if err == nil {
		t.Fatal("expected error for unset directory")
	}
	if !strings.Contains(res.Body, "尚未设置工作目录") {
		t.Errorf("body = %q, want usage hint", res.Body)
	}
	if called, _, _, _ := cmd.snapshot(); called {
		t.Error("git must not be invoked without a bound directory")
	}
}

// TestCmdPull_WithDirectory verifies the bound directory and args are
// forwarded to git once a /cd pin exists, and Handled=true so the
// dispatcher does not double-emit a notice.
func TestCmdPull_WithDirectory(t *testing.T) {
	cmd := &gitFakeCommander{out: []byte("Already up to date.\n")}
	h, r := newGitTestHandler(t, cmd)
	r.Bind("chat-1", "", "/repo/proj", "", "", "")

	res, err := h.cmdPull(context.Background(), "chat-1", nil)
	if err != nil {
		t.Fatalf("cmdPull: %v", err)
	}
	if !res.Handled {
		t.Errorf("expected Handled=true so dispatcher skips its own notice")
	}
	waitForCalled(t, cmd)

	called, dir, name, args := cmd.snapshot()
	if !called {
		t.Fatal("git not invoked")
	}
	if dir != "/repo/proj" {
		t.Errorf("git dir = %q, want /repo/proj", dir)
	}
	if name != "git" || len(args) != 2 || args[0] != "pull" || args[1] != "--ff-only" {
		t.Errorf("git args = %v %v, want [git pull --ff-only]", name, args)
	}
}

// TestCmdPush_WithDirectory verifies /push forwards `git push` to the
// bound directory.
func TestCmdPush_WithDirectory(t *testing.T) {
	cmd := &gitFakeCommander{out: []byte("ok")}
	h, r := newGitTestHandler(t, cmd)
	r.Bind("chat-2", "", "/repo/other", "", "", "")

	_, err := h.cmdPush(context.Background(), "chat-2", nil)
	if err != nil {
		t.Fatalf("cmdPush: %v", err)
	}
	waitForCalled(t, cmd)

	_, dir, _, args := cmd.snapshot()
	if len(args) != 1 || args[0] != "push" {
		t.Errorf("git args = %v, want [push]", args)
	}
	if dir != "/repo/other" {
		t.Errorf("git dir = %q, want /repo/other", dir)
	}
}
