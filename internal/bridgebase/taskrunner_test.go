package bridgebase

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
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

// noticeCapture collects the (level,title,body) triples handed to NoticeFn.
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

// TestAcquireAndRun_Success verifies the happy path: AcquireAndRun returns
// true on accept (the caller emits its own running banner), git runs in the
// bound dir, and the terminal "完成" notice carries git's output.
func TestAcquireAndRun_Success(t *testing.T) {
	cmd := &recordingCommander{out: []byte("Already up to date.\n")}
	r := NewTaskRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	if accepted := r.AcquireAndRun("chat-A", "/repo/proj", "git", []string{"pull", "--ff-only"}, "拉取", notices.fn); !accepted {
		t.Fatalf("AcquireAndRun returned false on a free slot")
	}
	if got := cmd.callCount(); got != 0 {
		t.Fatalf("Run should run async, got %d sync calls", got)
	}
	// The runner no longer emits a "triggered" notice — the caller owns the
	// non-terminal running banner. Only terminal notices fire from here.
	if _, ok := notices.findNotice("拉取已触发"); ok {
		t.Errorf("runner must not emit triggered; caller owns the banner; got %+v", notices.snapshot())
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
	r := NewTaskRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-B", "/repo", "git", []string{"push"}, "推送", notices.fn)

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
	r := NewTaskRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-X", "/r", "git", []string{"push"}, "推送", notices.fn)
	waitForCount(t, cmd, 1) // first job is now blocked inside Run

	// Second fire on the SAME chat must be rejected synchronously (returns
	// false + a 进行中 notice, zero new goroutine).
	if accepted := r.AcquireAndRun("chat-X", "/r", "git", []string{"push"}, "推送", notices.fn); accepted {
		t.Error("second AcquireAndRun on a busy chat should return false")
	}
	if n, ok := notices.findNotice("推送进行中"); !ok {
		t.Fatalf("expected 进行中 rejection; got %+v", notices.snapshot())
	} else if n.level != "warning" {
		t.Errorf("rejection level = %q, want warning", n.level)
	}

	// A DIFFERENT chat is unaffected: it gets its own slot and starts. The
	// runner no longer emits "triggered"; the caller would emit the banner,
	// so here we only assert the job actually starts.
	noticesY := &noticeCapture{}
	r.AcquireAndRun("chat-Y", "/r", "git", []string{"push"}, "推送", noticesY.fn)
	waitForCount(t, cmd, 2)

	// Release both jobs so goroutines exit and the test does not leak.
	close(cmd.release)
	waitForCount(t, cmd, 2)
}

// TestAcquireAndRun_TailOutputTruncation ensures a verbose git output is
// capped so the notice card stays scannable.
func TestAcquireAndRun_TailOutputTruncation(t *testing.T) {
	big := strings.Repeat("x", gitTailRunes*3)
	cmd := &recordingCommander{out: []byte(big)}
	r := NewTaskRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-T", "/r", "git", []string{"pull"}, "拉取", notices.fn)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, ok := notices.findNotice("拉取完成"); ok {
			// ASCII: 1 rune == 1 byte, so the rune budget bounds byte length too.
			wantMax := gitTailRunes + len("…")
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

// TestTailGitOutput_RuneAndLineAware pins the truncation contract: a
// multi-byte (Chinese) log is sized in runes so no character is split, and the
// excerpt opens at a line boundary so it never starts on a half-line fragment.
func TestTailGitOutput_RuneAndLineAware(t *testing.T) {
	// Each line "行NNN\n" is 4+ runes; build 200 lines and cap at 100 runes.
	// The pure byte form would split the 3-byte "行".
	var sb strings.Builder
	for i := range 200 {
		sb.WriteString("行" + strconv.Itoa(i) + "\n")
	}
	got := tailGitOutput([]byte(sb.String()))
	if !strings.HasPrefix(got, "…") {
		t.Errorf("truncated tail should start with …; got %q", got[:8])
	}
	if strings.Contains(got[:4], "�") {
		t.Errorf("tail split a multi-byte rune: %q", got[:8])
	}
	// Must start at a line boundary (first char after "…" is a whole "行").
	if after := strings.TrimPrefix(got, "…"); !strings.HasPrefix(after, "行") {
		t.Errorf("tail should open on a line boundary starting with 行; got %q", after[:4])
	}
	if r := len([]rune(got)); r > gitTailRunes+1 {
		t.Errorf("tail rune count = %d, want <= %d+1", r, gitTailRunes)
	}

	// Short input is returned verbatim (TrimSpace only).
	if got := tailGitOutput([]byte("Already up to date.\n")); got != "Already up to date." {
		t.Errorf("short input should pass through; got %q", got)
	}
}

// TestAcquireAndRun_SlotReleasedAfterJob verifies the per-chat slot frees
// up when the job finishes, so a subsequent /pull on the same chat is
// accepted rather than rejected as busy.
func TestAcquireAndRun_SlotReleasedAfterJob(t *testing.T) {
	cmd := &recordingCommander{out: []byte("ok")}
	r := NewTaskRunner(cmd, log.Nop(), 0)
	notices := &noticeCapture{}

	r.AcquireAndRun("chat-R", "/r", "git", []string{"pull"}, "拉取", notices.fn)
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

	// Second fire must NOT be rejected: returns true and starts a new job.
	notices2 := &noticeCapture{}
	if accepted := r.AcquireAndRun("chat-R", "/r", "git", []string{"pull"}, "拉取", notices2.fn); !accepted {
		t.Errorf("second fire after completion should be accepted; got rejected")
	}
	waitForCount(t, cmd, 2)
	if _, ok := notices2.findNotice("拉取进行中"); ok {
		t.Errorf("slot should be free after job done; got busy rejection")
	}
}

// TestNewTaskRunner_Defaults verifies timeout<=0 falls back to the default
// and a nil logger does not panic on the rejection path.
func TestNewTaskRunner_Defaults(t *testing.T) {
	r := NewTaskRunner(&recordingCommander{}, nil, 0)
	if r.timeout != defaultTaskTimeout {
		t.Errorf("timeout = %v, want default %v", r.timeout, defaultTaskTimeout)
	}
	if r.logger == nil {
		t.Error("nil logger should be replaced with no-op")
	}
	// Sanity: logger.Info must not panic on the rejection path.
	// First fire grabs the slot but blocks, second hits the busy path.
	blockCmd := &recordingCommander{release: make(chan struct{})}
	r.cmd = blockCmd
	notices := &noticeCapture{}
	r.AcquireAndRun("c", "/r", "git", []string{"push"}, "推送", notices.fn)
	waitForCount(t, blockCmd, 1)
	r.AcquireAndRun("c", "/r", "git", []string{"push"}, "推送", notices.fn) // logs "rejected"
	close(blockCmd.release)
}
