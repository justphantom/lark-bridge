package deploymonitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
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
	last     []string // last invocation: [name, args...]
	delay    time.Duration
	out      []byte
	err      error
	cancelCh chan struct{}
}

func (f *fakeCommander) Run(ctx context.Context, _ string, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	f.last = append([]string{name}, args...)
	f.mu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return f.out, ctx.Err()
		}
	}
	if f.cancelCh != nil {
		// block until test releases, simulating a long job
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

// lastCmd returns [name, args...] of the most recent Run, or nil if uncalled.
func (f *fakeCommander) lastCmd() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// fakeStatusQuerier returns a canned StatusSnapshot (or err) for /running
// tests. A nil snap yields an empty snapshot so the default newHandler wiring
// does not panic when a test never touches /running.
type fakeStatusQuerier struct {
	snap *protocol.StatusSnapshot
	err  error
}

func (f *fakeStatusQuerier) Status(_ context.Context) (*protocol.StatusSnapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.snap == nil {
		return &protocol.StatusSnapshot{}, nil
	}
	return f.snap, nil
}

func newHandler(rpc controlSender, cmd Commander) *Handler {
	return New(Config{ProjectRoot: "/repo", DeployTarget: "deploy"}, rpc, &fakeStatusQuerier{}, cmd, nil, 0)
}

func promptEvent(chatID, text string) *protocol.Event {
	// PromptID is stamped so tests exercise the promptID-bound notice path
	// (production events carry the frontend's message ID).
	return &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-" + chatID,
		Prompt:   &protocol.PromptPayload{ChatID: chatID, Text: text},
	}
}

// promptEventWithCard attaches the frontend's progress-card message_id, as a
// production event from feishu-front does (dispatcher.go).
func promptEventWithCard(chatID, text, cardMsgID string) *protocol.Event {
	ev := promptEvent(chatID, text)
	ev.Prompt.CardMessageID = cardMsgID
	return ev
}

// TestHandleEvent_TerminalNoticeCarriesCardMessageID pins the deploy-card
// mechanism: the terminal notice must echo the progress card's message_id as
// UpdateMessageID so the frontend can patch THAT card even after a /deploy
// restarted it (wiping the promptID→turn map).
func TestHandleEvent_TerminalNoticeCarriesCardMessageID(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("deployed")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEventWithCard("cc", "/deploy", "om_progress")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	for range 100 {
		if len(rpc.snapshot()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	all := rpc.snapshot()
	if len(all) < 2 {
		t.Fatalf("want banner + terminal notice, got %d controls", len(all))
	}
	terminal := all[len(all)-1]
	if terminal.Type != protocol.TypeNotice || terminal.Notice == nil {
		t.Fatalf("terminal control is not a notice: %+v", terminal)
	}
	if terminal.Notice.UpdateMessageID != "om_progress" {
		t.Errorf("UpdateMessageID = %q, want om_progress", terminal.Notice.UpdateMessageID)
	}
	if terminal.Notice.Level != "success" {
		t.Errorf("level = %q, want success", terminal.Notice.Level)
	}
}

func TestHandleEvent_DeployTriggersAndNotices(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("build ok\nall services started")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("c1", "/deploy")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	// Immediate emit is now a non-terminal TypeProgress banner bound to the
	// promptID (single-card lifecycle: no separate "triggered" card).
	immediate := rpc.snapshot()
	if len(immediate) != 1 || immediate[0].Type != protocol.TypeProgress {
		t.Fatalf("expected one immediate TypeProgress banner, got %+v", immediate)
	}
	if immediate[0].PromptID != "msg-c1" {
		t.Errorf("banner PromptID = %q, want msg-c1", immediate[0].PromptID)
	}
	if immediate[0].Progress == nil || !strings.Contains(immediate[0].Progress.Description, "部署") {
		t.Errorf("banner description = %+v, want contains '部署'", immediate[0].Progress)
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
	if terminal.Type != protocol.TypeNotice {
		t.Fatalf("terminal want TypeNotice, got %s", terminal.Type)
	}
	if terminal.PromptID != "msg-c1" {
		t.Errorf("terminal PromptID = %q, want msg-c1 (must bind to patch card in place)", terminal.PromptID)
	}
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

// TestHandleEvent_Running_ListsTurns verifies /running renders the frontend's
// in-flight snapshot: count, each turn's backend id, an elapsed label, and the
// abort hint reinforcing the "no auto-end" policy.
func TestHandleEvent_Running_ListsTurns(t *testing.T) {
	sender := &fakeSender{}
	snap := &protocol.StatusSnapshot{Turns: []protocol.TurnInfo{
		{BackendID: "claude-1", ChatID: "oc_d186b4d6aaaaaaaaaaaaaaaaaaaaaaaa", ElapsedS: 45},
		{BackendID: "opencode-1", ChatID: "oc_9cbbf3ebbbbbbbbbbbbbbbbbbbbbbbbb", ElapsedS: 130},
	}}
	h := New(Config{}, sender, &fakeStatusQuerier{snap: snap}, &fakeCommander{}, nil, 0)

	if err := h.HandleEvent(context.Background(), promptEvent("oc_x", "/running")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	body := lastNoticeMessage(sender.snapshot())
	for _, want := range []string{"运行中会话（2）", "claude-1", "opencode-1", "2m10s", "45s", "/session-abort"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandleEvent_Running_Empty renders a friendly line when nothing is running.
func TestHandleEvent_Running_Empty(t *testing.T) {
	sender := &fakeSender{}
	h := New(Config{}, sender, &fakeStatusQuerier{snap: &protocol.StatusSnapshot{}}, &fakeCommander{}, nil, 0)
	if err := h.HandleEvent(context.Background(), promptEvent("oc_x", "/running")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if body := lastNoticeMessage(sender.snapshot()); !strings.Contains(body, "当前没有运行中的会话") {
		t.Errorf("want empty-hint, got: %s", body)
	}
}

// TestHandleEvent_Running_Error surfaces a status-query failure as an error notice.
func TestHandleEvent_Running_Error(t *testing.T) {
	sender := &fakeSender{}
	h := New(Config{}, sender, &fakeStatusQuerier{err: errors.New("frontend down")}, &fakeCommander{}, nil, 0)
	if err := h.HandleEvent(context.Background(), promptEvent("oc_x", "/running")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if body := lastNoticeMessage(sender.snapshot()); !strings.Contains(body, "frontend down") {
		t.Errorf("want error echoed, got: %s", body)
	}
}

// lastNoticeMessage returns the message of the most recent captured Notice.
func lastNoticeMessage(ctrls []*protocol.Control) string {
	for i := len(ctrls) - 1; i >= 0; i-- {
		if ctrls[i].Notice != nil {
			return ctrls[i].Notice.Message
		}
	}
	return ""
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
		t.Errorf("maxRunes=0 want full output, got %q", got)
	}
	long := strings.Repeat("x", 200)
	got := tailOutput([]byte(long), 50)
	// "…" is 3 UTF-8 bytes + 50-byte tail = 53.
	if len(got) != 53 || !strings.HasPrefix(got, "…") {
		t.Errorf("want 53-byte '…'+tail, got len=%d prefix=%q", len(got), got[:1])
	}
}

// TestTailOutput_RuneAndLineAware pins the multi-byte + line-boundary
// contract: a Chinese log sized by rune does not split a 3-byte char, and the
// excerpt opens at a line boundary (no half-line fragment).
func TestTailOutput_RuneAndLineAware(t *testing.T) {
	// 6 runes per line ("行NNN\n"); 200 lines = 1200 runes; cap at 30.
	var sb strings.Builder
	for i := range 200 {
		sb.WriteString("行" + strconv.Itoa(i) + "\n")
	}
	got := tailOutput([]byte(sb.String()), 30)
	if !strings.HasPrefix(got, "…") {
		t.Errorf("truncated tail should start with …; got %q", got[:4])
	}
	after := strings.TrimPrefix(got, "…")
	if !strings.HasPrefix(after, "行") {
		t.Errorf("tail must open at a line boundary (行…); got %q", after[:4])
	}
	if strings.Contains(after, "�") {
		t.Errorf("tail split a multi-byte rune: %q", after[:4])
	}
}

func TestHandleEvent_PullRunsGitFFOnly(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("Already up to date.")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("c6", "/pull")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Join(cmd.lastCmd(), " "); got != "git pull --ff-only" {
		t.Fatalf("pull want 'git pull --ff-only', got %q", got)
	}
	// terminal notice arrives async; poll up to 1s.
	termDeadline := time.Now().Add(time.Second)
	for len(rpc.snapshot()) < 2 && time.Now().Before(termDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(lastNoticeMessage(rpc.snapshot()), "Already up to date.") {
		t.Errorf("pull notice should carry git output, got %q", lastNoticeMessage(rpc.snapshot()))
	}
}

func TestHandleEvent_PushRunsGitPush(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("pushed")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("c7", "/push")); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Join(cmd.lastCmd(), " "); got != "git push" {
		t.Errorf("push want 'git push', got %q", got)
	}
}

// TestHandleEvent_JobsShareSingleFlightSlot verifies /deploy, /pull and /push
// share one slot: a /pull while a /deploy is in flight is rejected, not queued.
func TestHandleEvent_JobsShareSingleFlightSlot(t *testing.T) {
	rpc := &fakeSender{}
	release := make(chan struct{})
	cmd := &fakeCommander{cancelCh: release}
	h := newHandler(rpc, cmd)

	_ = h.HandleEvent(context.Background(), promptEvent("c8", "/deploy"))
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	_ = h.HandleEvent(context.Background(), promptEvent("c8", "/pull"))
	if cmd.callCount() != 1 {
		t.Fatalf("/pull during deploy must not run, calls=%d", cmd.callCount())
	}
	var sawInProgress bool
	for _, c := range rpc.snapshot() {
		if c.Notice != nil && strings.Contains(c.Notice.Title, "进行中") {
			sawInProgress = true
		}
	}
	if !sawInProgress {
		t.Errorf("expected 'in progress' notice for /pull during deploy, got %+v", rpc.snapshot())
	}
	close(release)
}

// findPermission returns the first TypePermission control captured, or nil.
func findPermission(ctrls []*protocol.Control) *protocol.Control {
	for _, c := range ctrls {
		if c.Type == protocol.TypePermission && c.Permission != nil {
			return c
		}
	}
	return nil
}

// answerEvent simulates the frontend forwarding a card click as a TypeAnswer.
func answerEvent(chatID, requestID, choice string) *protocol.Event {
	return &protocol.Event{
		Type: protocol.TypeAnswer,
		Answer: &protocol.AnswerPayload{
			ChatID:    chatID,
			RequestID: requestID,
			Choice:    choice,
		},
	}
}

// TestHandleEvent_DeployForce_ConfirmRuns waits for the confirm card, delivers
// a "confirm" click, and asserts make deploy ARGS=--force actually runs.
func TestHandleEvent_DeployForce_ConfirmRuns(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("deployed")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("cf", "/deploy-force")); err != nil {
		t.Fatalf("HandleEvent /deploy-force: %v", err)
	}
	// The deploy must NOT start before the user confirms.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cmd.callCount() > 0 {
			t.Fatalf("deploy ran before confirm: calls=%d", cmd.callCount())
		}
		time.Sleep(5 * time.Millisecond)
		if findPermission(rpc.snapshot()) != nil {
			break
		}
	}
	perm := findPermission(rpc.snapshot())
	if perm == nil {
		t.Fatalf("expected a TypePermission confirm card, got %+v", rpc.snapshot())
	}
	if perm.Permission.RequestID != "msg-cf" || perm.PromptID != "msg-cf" {
		t.Errorf("confirm card requestID/promptID = %q/%q, want msg-cf/msg-cf",
			perm.Permission.RequestID, perm.PromptID)
	}

	// Deliver the confirm click; the deploy should run with ARGS=--force.
	if err := h.HandleEvent(context.Background(), answerEvent("cf", "msg-cf", "confirm")); err != nil {
		t.Fatalf("HandleEvent answer: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Join(cmd.lastCmd(), " "); got != "make deploy ARGS=--force" {
		t.Errorf("confirm should run `make deploy ARGS=--force`, got %q", got)
	}
}

// TestHandleEvent_DeployForce_CancelDoesNotRun verifies a "cancel" click
// releases the wait WITHOUT running make, and emits an "已取消" notice.
func TestHandleEvent_DeployForce_CancelDoesNotRun(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("deployed")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("cx", "/deploy-force")); err != nil {
		t.Fatalf("HandleEvent /deploy-force: %v", err)
	}
	// Wait for the confirm card so the await goroutine has registered.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && findPermission(rpc.snapshot()) == nil {
		time.Sleep(5 * time.Millisecond)
	}

	if err := h.HandleEvent(context.Background(), answerEvent("cx", "msg-cx", "cancel")); err != nil {
		t.Fatalf("HandleEvent answer: %v", err)
	}
	// Give the await goroutine a moment to emit the cancel notice.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cmd.callCount() > 0 {
			t.Fatalf("cancel must NOT run make, got calls=%d", cmd.callCount())
		}
		if strings.Contains(lastNoticeMessage(rpc.snapshot()), "已取消") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected 已取消 notice and zero make calls; calls=%d notices=%+v",
		cmd.callCount(), rpc.snapshot())
}

// findQuestion returns the first TypeQuestion control captured, or nil.
func findQuestion(ctrls []*protocol.Control) *protocol.Control {
	for _, c := range ctrls {
		if c.Type == protocol.TypeQuestion && c.Question != nil {
			return c
		}
	}
	return nil
}

// answerEventChoices simulates the frontend forwarding a multi-select question
// card submission as a TypeAnswer carrying Choices (the multi-select path).
func answerEventChoices(chatID, requestID string, choices []string) *protocol.Event {
	return &protocol.Event{
		Type: protocol.TypeAnswer,
		Answer: &protocol.AnswerPayload{
			ChatID:    chatID,
			RequestID: requestID,
			Choices:   choices,
		},
	}
}

// waitForQuestion polls captured controls for the /deploy-some picker card so
// the test delivers its answer only after the await goroutine has registered.
func waitForQuestion(rpc *fakeSender) *protocol.Control {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if q := findQuestion(rpc.snapshot()); q != nil {
			return q
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil
}

// TestHandleEvent_DeploySome_EmitsMultiSelectCard pins the card shape: a
// TypeQuestion with Multiple=true and the four business-service options.
func TestHandleEvent_DeploySome_EmitsMultiSelectCard(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("cs", "/deploy-some")); err != nil {
		t.Fatalf("HandleEvent /deploy-some: %v", err)
	}
	q := waitForQuestion(rpc)
	if q == nil {
		t.Fatalf("expected a TypeQuestion picker card, got %+v", rpc.snapshot())
	}
	if q.Question.RequestID != "msg-cs" || q.PromptID != "msg-cs" {
		t.Errorf("picker requestID/promptID = %q/%q, want msg-cs/msg-cs",
			q.Question.RequestID, q.PromptID)
	}
	if len(q.Question.Questions) != 1 {
		t.Fatalf("want one question item, got %d", len(q.Question.Questions))
	}
	item := q.Question.Questions[0]
	if !item.Multiple {
		t.Errorf("question item must be Multiple=true for /deploy-some")
	}
	if got := strings.Join(item.Options, ","); got != "feishu,claude,opencode,miniagent" {
		t.Errorf("options = %q, want feishu,claude,opencode,miniagent", got)
	}
	// Deploy must NOT run before the user submits.
	if cmd.callCount() > 0 {
		t.Fatalf("deploy ran before submit: calls=%d", cmd.callCount())
	}
	// Release the picker goroutine so it does not leak for confirmTimeout.
	_ = h.HandleEvent(context.Background(), answerEventChoices("cs", "msg-cs", nil))
}

// TestHandleEvent_DeploySome_MultiSelectRuns delivers a [feishu,claude] pick
// and asserts make deploy ARGS=--services=feishu,claude actually runs.
func TestHandleEvent_DeploySome_MultiSelectRuns(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("deployed")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("cs", "/deploy-some")); err != nil {
		t.Fatalf("HandleEvent /deploy-some: %v", err)
	}
	if waitForQuestion(rpc) == nil {
		t.Fatalf("picker card never emitted")
	}
	if err := h.HandleEvent(context.Background(), answerEventChoices("cs", "msg-cs", []string{"feishu", "claude"})); err != nil {
		t.Fatalf("HandleEvent answer: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := strings.Join(cmd.lastCmd(), " "); got != "make deploy ARGS=--services=feishu,claude" {
		t.Errorf("submit should run `make deploy ARGS=--services=feishu,claude`, got %q", got)
	}
}

// TestHandleEvent_DeploySome_EmptyCancel verifies an empty submission releases
// the wait WITHOUT running make, emitting an "已取消" notice.
func TestHandleEvent_DeploySome_EmptyCancel(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("deployed")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("ce", "/deploy-some")); err != nil {
		t.Fatalf("HandleEvent /deploy-some: %v", err)
	}
	if waitForQuestion(rpc) == nil {
		t.Fatalf("picker card never emitted")
	}
	if err := h.HandleEvent(context.Background(), answerEventChoices("ce", "msg-ce", nil)); err != nil {
		t.Fatalf("HandleEvent answer: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cmd.callCount() > 0 {
			t.Fatalf("empty submit must NOT run make, got calls=%d", cmd.callCount())
		}
		if strings.Contains(lastNoticeMessage(rpc.snapshot()), "已取消") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected 已取消 notice and zero make calls; calls=%d notices=%+v",
		cmd.callCount(), rpc.snapshot())
}

// TestHandleEvent_DeploySome_UnknownService verifies an unknown service name
// fails fast without running make.
func TestHandleEvent_DeploySome_UnknownService(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{out: []byte("deployed")}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("cu", "/deploy-some")); err != nil {
		t.Fatalf("HandleEvent /deploy-some: %v", err)
	}
	if waitForQuestion(rpc) == nil {
		t.Fatalf("picker card never emitted")
	}
	if err := h.HandleEvent(context.Background(), answerEventChoices("cu", "msg-cu", []string{"feishu", "bogus"})); err != nil {
		t.Fatalf("HandleEvent answer: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cmd.callCount() > 0 {
			t.Fatalf("unknown-service submit must NOT run make, got calls=%d", cmd.callCount())
		}
		body := lastNoticeMessage(rpc.snapshot())
		if strings.Contains(body, "未知服务") && strings.Contains(body, "bogus") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected 选择无效 notice mentioning bogus; calls=%d notices=%+v",
		cmd.callCount(), rpc.snapshot())
}

// TestHandleEvent_DeploySome_WaitDoesNotOccupySlot verifies the picker wait
// does NOT take the single-flight slot: while the card is pending h.running
// stays false. The slot is only claimed on submit (see BusyOnSubmit).
func TestHandleEvent_DeploySome_WaitDoesNotOccupySlot(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{}
	h := newHandler(rpc, cmd)

	if err := h.HandleEvent(context.Background(), promptEvent("cw", "/deploy-some")); err != nil {
		t.Fatalf("HandleEvent /deploy-some: %v", err)
	}
	if waitForQuestion(rpc) == nil {
		t.Fatalf("picker card never emitted")
	}
	if h.running {
		t.Errorf("picker wait must NOT occupy the single-flight slot")
	}
	// Cancel the picker so its goroutine exits cleanly instead of leaking.
	if err := h.HandleEvent(context.Background(), answerEventChoices("cw", "msg-cw", nil)); err != nil {
		t.Fatalf("HandleEvent answer: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cmd.callCount() > 0 {
			t.Fatalf("cancel must not run make")
		}
		if strings.Contains(lastNoticeMessage(rpc.snapshot()), "已取消") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestHandleEvent_DeploySome_BusyOnSubmit verifies that submitting the picker
// while another job holds the slot is rejected: the /deploy-some make does
// NOT run and an "进行中" notice is emitted by acquireAndRun.
func TestHandleEvent_DeploySome_BusyOnSubmit(t *testing.T) {
	rpc := &fakeSender{}
	release := make(chan struct{})
	cmd := &fakeCommander{cancelCh: release}
	h := newHandler(rpc, cmd)

	// /deploy grabs the slot and blocks.
	if err := h.HandleEvent(context.Background(), promptEvent("cb", "/deploy")); err != nil {
		t.Fatalf("HandleEvent /deploy: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	// /deploy-some picker waits (does not need the slot to show its card).
	if err := h.HandleEvent(context.Background(), promptEvent("cb", "/deploy-some")); err != nil {
		t.Fatalf("HandleEvent /deploy-some: %v", err)
	}
	if waitForQuestion(rpc) == nil {
		t.Fatalf("picker card never emitted")
	}
	before := cmd.callCount()
	if err := h.HandleEvent(context.Background(), answerEventChoices("cb", "msg-cb", []string{"feishu"})); err != nil {
		t.Fatalf("HandleEvent answer: %v", err)
	}
	// The submit must NOT start a second make; the busy notice must appear.
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cmd.callCount() > before {
			close(release)
			t.Fatalf("submit while busy must NOT run make, calls=%d", cmd.callCount())
		}
		for _, c := range rpc.snapshot() {
			if c.Notice != nil && strings.Contains(c.Notice.Title, "进行中") {
				close(release)
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	t.Fatalf("expected 进行中 notice on busy submit; calls=%d notices=%+v",
		cmd.callCount(), rpc.snapshot())
}

// TestConfirmTimeoutCoversCardLifetime locks in the P1 fix: the picker card
// advertises cardkit.InteractiveTimeout ("⏳ 等待你的确认（10 分钟后自动失效）"),
// so the backend MUST wait at least that long. If confirmTimeout fell below
// the card's lifetime, a submission in the gap would be silently dropped (the
// AnswerBroker slot would already be gone).
func TestConfirmTimeoutCoversCardLifetime(t *testing.T) {
	if confirmTimeout < cardkit.InteractiveTimeout {
		t.Errorf("confirmTimeout (%s) must be >= cardkit.InteractiveTimeout (%s); "+
			"otherwise submissions between the two are silently dropped",
			confirmTimeout, cardkit.InteractiveTimeout)
	}
}

// TestAcquireAndRun_ProgressFailRollsBackSlot verifies the h.running rollback
// when the progress banner POST fails. Without the rollback, runJob never
// starts (so its defer that clears h.running never runs) and a single failed
// banner POST would wedge single-flight forever — every later /deploy / /pull
// / /push rejected as 进行中. The banner ctx is self-derived (P2), so a stale
// picker-wait deadline can no longer trigger this, but a transport failure
// still can; the rollback must hold in that case too.
func TestAcquireAndRun_ProgressFailRollsBackSlot(t *testing.T) {
	rpc := &fakeSender{notifyErr: errors.New("frontend 503")}
	cmd := &fakeCommander{}
	h := newHandler(rpc, cmd)

	if err := h.acquireAndRun("chat", "p1", "card", "make", []string{"deploy"}, "部署"); err == nil {
		t.Fatal("acquireAndRun must fail when the progress banner POST fails")
	}
	if h.running {
		t.Fatal("h.running must roll back to false when the banner POST fails (else single-flight wedges)")
	}

	// The slot being free, a second acquireAndRun must proceed (not busy). Clear
	// the transport error so the banner succeeds and the job actually runs.
	rpc.notifyErr = nil
	if err := h.acquireAndRun("chat", "p2", "card2", "make", []string{"deploy"}, "部署"); err != nil {
		t.Fatalf("second acquireAndRun after rollback should proceed, got: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cmd.callCount() == 0 {
		t.Error("second acquireAndRun's job never ran — slot was not actually freed by the rollback")
	}
}

// TestAcquireAndRun_BusyIsNotAnError locks in the P3 fix: a busy rejection
// returns nil (the 进行中 notice was sent best-effort), NOT the notice's
// error. Previously callers mislabelled a failed busy-notice as "部署失败".
func TestAcquireAndRun_BusyIsNotAnError(t *testing.T) {
	rpc := &fakeSender{}
	cmd := &fakeCommander{cancelCh: make(chan struct{})}
	h := newHandler(rpc, cmd)

	// First call grabs the slot and blocks (job waits on cancelCh).
	if err := h.acquireAndRun("chat", "p1", "card", "make", []string{"deploy"}, "部署"); err != nil {
		t.Fatalf("first acquireAndRun: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for cmd.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if cmd.callCount() == 0 {
		close(cmd.cancelCh)
		t.Fatal("first job never started")
	}

	// Second call is rejected as busy; it must return nil even though the
	// busy-notice transport may fail (here it succeeds).
	if err := h.acquireAndRun("chat", "p2", "card2", "make", []string{"deploy"}, "部署"); err != nil {
		t.Errorf("busy rejection must return nil (not surface to callers as 部署失败), got: %v", err)
	}
	if !h.running {
		t.Error("h.running should still be true (first job still holds the slot)")
	}
	close(cmd.cancelCh)
}
