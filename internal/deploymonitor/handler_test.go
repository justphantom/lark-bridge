package deploymonitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// fakeSender captures SendControl calls. Each Control is appended to mu.captured
// under a mutex so the async deploy goroutine and the test can read concurrently.
type fakeSender struct {
	mu        sync.Mutex
	captured  []*protocol.Control
	notifyErr error
}

func (f *fakeSender) SendControl(_ context.Context, ctrl *protocol.Control) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	clone := *ctrl
	if ctrl.Notice != nil {
		n := *ctrl.Notice
		clone.Notice = &n
	}
	f.captured = append(f.captured, &clone)
	return f.notifyErr
}

func (f *fakeSender) snapshot() []*protocol.Control {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*protocol.Control, len(f.captured))
	copy(out, f.captured)
	return out
}

// fakeCommander records calls and returns the configured output/err. delay
// lets tests control ordering when asserting single-flight rejection.
type fakeCommander struct {
	mu       sync.Mutex
	calls    int
	delay    time.Duration
	out      []byte
	err      error
	cancelCh chan struct{}
}

func (f *fakeCommander) Run(ctx context.Context, _, _ string, _ ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return f.out, ctx.Err()
		}
	}
	if f.cancelCh != nil {
		// block until test releases, simulating a long deploy
		select {
		case <-f.cancelCh:
		case <-ctx.Done():
			return f.out, ctx.Err()
		}
	}
	return f.out, f.err
}

func (f *fakeCommander) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func newHandler(rpc controlSender, cmd Commander) *Handler {
	return New(Config{ProjectRoot: "/repo", DeployTarget: "deploy"}, rpc, cmd, nil, 0)
}

func promptEvent(chatID, text string) *protocol.Event {
	return &protocol.Event{
		Type:   protocol.TypePrompt,
		Prompt: &protocol.PromptPayload{ChatID: chatID, Text: text},
	}
}

func TestHandleEvent_DeployTriggersAndNotices(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("build ok\nall services started")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("c1", "/deploy")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Immediate "triggered" notice is sent synchronously.
	immediate := rpc.snapshot()
	if len(immediate) != 1 || immediate[0].Notice.Level != "info" {
		t.Fatalf("expected one immediate info notice, got %+v", immediate)
	}

	// Terminal notice arrives after the async deploy; poll up to 1s.
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() != 1 || len(rpc.snapshot()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("deploy did not complete: calls=%d notices=%d",
				cmd.callCount(), len(rpc.snapshot()))
		}
		time.Sleep(5 * time.Millisecond)
	}

	all := rpc.snapshot()
	terminal := all[len(all)-1]
	if terminal.ChatID != "c1" || terminal.Notice.Level != "success" {
		t.Errorf("terminal notice want c1/success, got %s/%s",
			terminal.ChatID, terminal.Notice.Level)
	}
	if !strings.Contains(terminal.Notice.Message, "all services started") {
		t.Errorf("terminal notice should carry deploy tail, got %q",
			terminal.Notice.Message)
	}
}

func TestHandleEvent_FailureEmitsError(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("partial output"), err: errors.New("exit 1")}
	h := newHandler(rpc, cmd)

	_ = h.HandleEvent(context.Background(), promptEvent("c2", "/deploy"))

	deadline := time.Now().Add(time.Second)
	for len(rpc.snapshot()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("no terminal notice, got %+v", rpc.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}

	terminal := rpc.snapshot()[1]
	if terminal.Notice.Level != "error" {
		t.Errorf("want error level, got %s", terminal.Notice.Level)
	}
	if !strings.Contains(terminal.Notice.Message, "exit 1") {
		t.Errorf("error notice should carry the error, got %q", terminal.Notice.Message)
	}
}

func TestHandleEvent_NonDeployRejected(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("c3", "/help me")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	// Give the async path a moment to ensure no stray deploy fires.
	time.Sleep(20 * time.Millisecond)

	notices := rpc.snapshot()
	if len(notices) != 1 {
		t.Fatalf("want exactly one rejection notice, got %+v", notices)
	}
	if notices[0].Notice.Level != "warning" {
		t.Errorf("want warning level, got %s", notices[0].Notice.Level)
	}
	if cmd.callCount() != 0 {
		t.Errorf("non-/deploy must not run deploy, got %d calls", cmd.callCount())
	}
}

func TestHandleEvent_SingleFlightRejectsConcurrent(t *testing.T) {
	rpc := &fakeSender{}
	release := make(chan struct{})
	cmd := &fakeCommander{cancelCh: release}
	h := newHandler(rpc, cmd)

	// First /deploy starts a deploy that blocks until we close `release`.
	if err := h.HandleEvent(context.Background(), promptEvent("c4", "/deploy")); err != nil {
		t.Fatalf("first HandleEvent: %v", err)
	}
	// Wait for the first deploy to enter run().
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if cmd.callCount() != 1 {
		t.Fatalf("first deploy did not start, calls=%d", cmd.callCount())
	}

	// Second /deploy while the first is still in flight must be rejected
	// synchronously without launching a second deploy.
	if err := h.HandleEvent(context.Background(), promptEvent("c4", "/deploy")); err != nil {
		t.Fatalf("second HandleEvent: %v", err)
	}
	if cmd.callCount() != 1 {
		t.Fatalf("second /deploy must not start another deploy, calls=%d", cmd.callCount())
	}

	// The second call should have produced an "in progress" warning notice.
	var sawInProgress bool
	for _, c := range rpc.snapshot() {
		if c.Notice != nil && strings.Contains(c.Notice.Title, "进行中") {
			sawInProgress = true
		}
	}
	if !sawInProgress {
		t.Errorf("expected a 'in progress' notice, got %+v", rpc.snapshot())
	}

	// Release the first deploy; running flag must clear so a later /deploy works.
	close(release)
	deadline = time.Now().Add(time.Second)
	for (cmd.callCount() != 1 || len(rpc.snapshot()) < 3) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	// After completion, a new /deploy should be accepted (single-flight cleared).
	beforeCalls := cmd.callCount()
	if err := h.HandleEvent(context.Background(), promptEvent("c4", "/deploy")); err != nil {
		t.Fatalf("third HandleEvent after completion: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for cmd.callCount() == beforeCalls && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cmd.callCount() != beforeCalls+1 {
		t.Errorf("single-flight flag did not clear after deploy: calls=%d (before=%d)",
			cmd.callCount(), beforeCalls)
	}
}

func TestHandleEvent_IgnoresNonPrompt(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{}
	h := newHandler(rpc, cmd)

	// Answer/Abort events are ignored without error or side effects.
	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:   protocol.TypeAnswer,
		Answer: &protocol.AnswerPayload{ChatID: "c5"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(rpc.snapshot()) != 0 || cmd.callCount() != 0 {
		t.Errorf("non-prompt events must be ignored, notices=%d calls=%d",
			len(rpc.snapshot()), cmd.callCount())
	}
}

func TestTailOutput(t *testing.T) {
	if got := tailOutput([]byte("hello"), 100); got != "hello" {
		t.Errorf("short input want 'hello', got %q", got)
	}
	if got := tailOutput([]byte("hello"), 0); got != "hello" {
		t.Errorf("maxBytes=0 want full output, got %q", got)
	}
	long := strings.Repeat("x", 200)
	got := tailOutput([]byte(long), 50)
	// "…" is 3 UTF-8 bytes + 50-byte tail = 53.
	if len(got) != 53 || !strings.HasPrefix(got, "…") {
		t.Errorf("want 53-byte '…'+tail, got len=%d prefix=%q", len(got), got[:1])
	}
}
