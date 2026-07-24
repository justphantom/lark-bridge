package bridgebase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// recordingCommander records each Run invocation and returns the configured
// out/err, optionally blocking on release until the test signals it. This
// lets single-flight tests deterministically hold a job mid-flight.
type recordingCommander struct {
	mu       sync.Mutex
	calls    int
	lastDir  string
	lastArgs []string
	out      []byte
	err      error
	release  chan struct{}
}

func (c *recordingCommander) Run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	c.mu.Lock()
	c.calls++
	c.lastDir = dir
	c.lastArgs = append([]string{name}, args...)
	c.mu.Unlock()
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return c.out, ctx.Err()
		}
	}
	return c.out, c.err
}

func (c *recordingCommander) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// noticeCapture collects the (level,title,body) triples handed to GitNotice.
type noticeCapture struct {
	mu      sync.Mutex
	notices []noticeEntry
}

type noticeEntry struct {
	level string
	title string
	body  string
}

func (n *noticeCapture) fn(level, title, body string) {
	n.mu.Lock()
	n.notices = append(n.notices, noticeEntry{level, title, body})
	n.mu.Unlock()
}

func (n *noticeCapture) snapshot() []noticeEntry {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]noticeEntry, len(n.notices))
	copy(out, n.notices)
	return out
}

// findNotice reports whether any captured notice title contains want.
func (n *noticeCapture) findNotice(want string) (noticeEntry, bool) {
	for _, e := range n.snapshot() {
		if strings.Contains(e.title, want) {
			return e, true
		}
	}
	return noticeEntry{}, false
}

// waitForCount polls until callCount reaches want or the deadline passes.
// Without it the test would race the goroutine's call to Run.
func waitForCount(t *testing.T, c *recordingCommander, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c.callCount() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("commander call count %d, want %d (timeout)", c.callCount(), want)
}

// TestAcquireAndRun_Success verifies the happy path: an immediate "已触发"
// notice fires synchronously, git runs in the bound dir, and the terminal
// "完成" notice carries git's output.
func TestAcquireAndRun_Success(t *testing.T) {
	cmd := &recordingCommander{out: []byte("Already up to date.\n")}
	r := NewGitRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-A", "/repo/proj", []string{"pull", "--ff-only"}, "拉取", notices.fn)

	if got := cmd.callCount(); got != 0 {
		t.Fatalf("Run should run async, got %d sync calls", got)
	}
	// The "triggered" notice is emitted synchronously before returning.
	if _, ok := notices.findNotice("拉取已触发"); !ok {
		t.Errorf("missing synchronous 已触发 notice; got %+v", notices.snapshot())
	}

	// dir/args are handed to git as-is.
	waitForCount(t, cmd, 1)
	cmd.mu.Lock()
	dir, args := cmd.lastDir, cmd.lastArgs
	cmd.mu.Unlock()
	if dir != "/repo/proj" {
		t.Errorf("Run dir = %q, want /repo/proj", dir)
	}
	if len(args) != 3 || args[0] != "git" || args[1] != "pull" || args[2] != "--ff-only" {
		t.Errorf("Run args = %v, want [git pull --ff-only]", args)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, ok := notices.findNotice("拉取完成"); ok {
			if !strings.Contains(n.body, "Already up to date") {
				t.Errorf("terminal body = %q, want git output", n.body)
			}
			if n.level != "success" {
				t.Errorf("terminal level = %q, want success", n.level)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("missing terminal 完成 notice; got %+v", notices.snapshot())
}

// TestAcquireAndRun_Failure verifies a non-zero git exit surfaces an
// error-level notice carrying both tail output and the error message.
func TestAcquireAndRun_Failure(t *testing.T) {
	cmd := &recordingCommander{
		out: []byte("error: failed to push some refs\ngit pull first"),
		err: errors.New("exit status 1"),
	}
	r := NewGitRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-B", "/repo", []string{"push"}, "推送", notices.fn)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, ok := notices.findNotice("推送失败"); ok {
			if n.level != "error" {
				t.Errorf("terminal level = %q, want error", n.level)
			}
			if !strings.Contains(n.body, "failed to push") || !strings.Contains(n.body, "exit status 1") {
				t.Errorf("terminal body = %q, want tail output + error", n.body)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("missing 失败 notice; got %+v", notices.snapshot())
}

// TestAcquireAndRun_PerChatSingleFlight pins the core invariant: while a
// job is mid-flight for chatID X, a second AcquireAndRun for X is rejected
// inline (a 进行中 notice, zero new goroutine) while a different chatID Y
// runs unhindered.
func TestAcquireAndRun_PerChatSingleFlight(t *testing.T) {
	cmd := &recordingCommander{release: make(chan struct{})}
	r := NewGitRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-X", "/r", []string{"push"}, "推送", notices.fn)
	waitForCount(t, cmd, 1) // first job is now blocked inside Run

	// Second fire on the SAME chat must be rejected synchronously.
	r.AcquireAndRun("chat-X", "/r", []string{"push"}, "推送", notices.fn)
	if n, ok := notices.findNotice("推送进行中"); !ok {
		t.Fatalf("expected 进行中 rejection; got %+v", notices.snapshot())
	} else if n.level != "warning" {
		t.Errorf("rejection level = %q, want warning", n.level)
	}

	// A DIFFERENT chat is unaffected: it gets its own slot and starts.
	noticesY := &noticeCapture{}
	r.AcquireAndRun("chat-Y", "/r", []string{"push"}, "推送", noticesY.fn)
	waitForCount(t, cmd, 2)
	if _, ok := noticesY.findNotice("推送已触发"); !ok {
		t.Errorf("chat-Y should trigger independently; got %+v", noticesY.snapshot())
	}

	// Release both jobs so goroutines exit and the test does not leak.
	close(cmd.release)
	waitForCount(t, cmd, 2)
}

// TestAcquireAndRun_TailOutputTruncation ensures a verbose git output is
// capped so the notice card stays scannable.
func TestAcquireAndRun_TailOutputTruncation(t *testing.T) {
	big := strings.Repeat("x", gitTailBytes*3)
	cmd := &recordingCommander{out: []byte(big)}
	r := NewGitRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-T", "/r", []string{"pull"}, "拉取", notices.fn)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, ok := notices.findNotice("拉取完成"); ok {
			wantMax := gitTailBytes + len("…")
			if len(n.body) > wantMax {
				t.Errorf("body len = %d, want <= %d", len(n.body), wantMax)
			}
			if !strings.HasPrefix(n.body, "…") {
				t.Errorf("truncated body should start with …; got %q", n.body[:10])
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("missing terminal notice; got %+v", notices.snapshot())
}

// TestAcquireAndRun_SlotReleasedAfterJob verifies the per-chat slot frees
// up when the job finishes, so a subsequent /pull on the same chat is
// accepted rather than rejected as busy.
func TestAcquireAndRun_SlotReleasedAfterJob(t *testing.T) {
	cmd := &recordingCommander{out: []byte("ok")}
	r := NewGitRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-R", "/r", []string{"pull"}, "拉取", notices.fn)
	waitForCount(t, cmd, 1)

	// Wait for the terminal notice so the goroutine has returned and
	// unlocked the slot.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := notices.findNotice("拉取完成"); ok {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// Second fire must NOT be rejected: a new "triggered" notice appears.
	notices2 := &noticeCapture{}
	r.AcquireAndRun("chat-R", "/r", []string{"pull"}, "拉取", notices2.fn)
	if _, ok := notices2.findNotice("拉取已触发"); !ok {
		t.Errorf("second fire after completion should trigger; got %+v", notices2.snapshot())
	}
	if _, ok := notices2.findNotice("拉取进行中"); ok {
		t.Errorf("slot should be free after job done; got busy rejection")
	}
}

// TestNewGitRunner_Defaults verifies timeout<=0 falls back to the default
// and a nil logger does not panic on the rejection path.
func TestNewGitRunner_Defaults(t *testing.T) {
	r := NewGitRunner(&recordingCommander{}, nil, 0)
	if r.timeout != defaultGitTimeout {
		t.Errorf("timeout = %v, want default %v", r.timeout, defaultGitTimeout)
	}
	if r.logger == nil {
		t.Error("nil logger should be replaced with no-op")
	}
	// Sanity: logger.Info must not panic on the rejection path.
	var blocked atomic.Bool
	// First fire grabs the slot but blocks, second hits the busy path.
	blockCmd := &recordingCommander{release: make(chan struct{})}
	r.cmd = blockCmd
	notices := &noticeCapture{}
	r.AcquireAndRun("c", "/r", []string{"push"}, "推送", notices.fn)
	waitForCount(t, blockCmd, 1)
	r.AcquireAndRun("c", "/r", []string{"push"}, "推送", notices.fn) // logs "rejected"
	close(blockCmd.release)
	if !blocked.CompareAndSwap(false, false) { // touch to silence unused
	}
}
