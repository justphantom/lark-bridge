package ompbridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// fakeAgent satisfies ompAPI for command tests. Run is the interface's only
// method and none of the commands under test invoke it; the closed channel
// keeps the fake honest if a future command does.
type fakeAgent struct{}

func (fakeAgent) Run(context.Context, omp.RunOptions) (<-chan omp.Event, error) {
	ch := make(chan omp.Event)
	close(ch)
	return ch, nil
}

// newTestHandler builds a Handler with nil rpc (emits are no-ops) so tests can
// drive interactive pickers straight through h.Answers and assert on router
// state. Option lists mirror config.example.json's omp defaults.
func newTestHandler(t *testing.T) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, fakeAgent{}, nil, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{
			PermissionDefault: "write",
		},
		ThinkingDefault: "auto",
		ModelOptions:    []string{"glm-5.2", "claude-haiku"},
		ApprovalOptions: []string{"always-ask", "write", "yolo"},
		ThinkingOptions: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"},
	}, log.Nop())
	t.Cleanup(h.Close)
	return h, r
}

// waitCond polls pred every 2ms until true or timeout. Used to wait for the
// AnswerBroker registration an interactive picker creates before delivering a
// canned answer.
func waitCond(timeout time.Duration, pred func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return pred()
}

// --- /perm picker (G5) ---

// TestCmdPermission_PickerRejectsInvalid verifies the /perm picker re-checks
// the delivered choice against isSettableApprovalMode and refuses to persist a
// value the CLI's --approval-mode rejects. Models the misconfig where
// approval_options leaks an illegal entry; without the guard the next omp run
// would fail at flag parse. The pin must stay empty and the command must
// return Handled (the picker card is patched in place with an error).
func TestCmdPermission_PickerRejectsInvalid(t *testing.T) {
	h, r := newTestHandler(t)

	done := make(chan commandResult, 1)
	go func() {
		res, _ := h.cmdPermission(context.Background(), "c1", nil) // picker path
		done <- res
	}()

	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) > 0 }) {
		t.Fatal("perm picker did not register a waiter")
	}
	reqID := h.Answers.PendingIDs()[0]
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"ASK"}}) // not a legal mode

	select {
	case res := <-done:
		if !res.Handled {
			t.Errorf("expected Handled=true, got %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("cmdPermission did not return after deliver")
	}
	if b, _ := r.Lookup("c1"); b.PermissionMode != "" {
		t.Errorf("PermissionMode = %q, want empty (invalid choice must not persist)", b.PermissionMode)
	}
}

// TestCmdPermission_PickerAcceptsValid is the happy-path regression guard so
// the new gate does not reject a legal mode delivered from the card.
func TestCmdPermission_PickerAcceptsValid(t *testing.T) {
	h, r := newTestHandler(t)

	done := make(chan struct{})
	go func() {
		_, _ = h.cmdPermission(context.Background(), "c1", nil)
		close(done)
	}()

	if !waitCond(time.Second, func() bool { return len(h.Answers.PendingIDs()) > 0 }) {
		t.Fatal("perm picker did not register a waiter")
	}
	reqID := h.Answers.PendingIDs()[0]
	h.Answers.Deliver(reqID, &protocol.AnswerPayload{Choices: []string{"yolo"}})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cmdPermission did not return after deliver")
	}
	if b, _ := r.Lookup("c1"); b.PermissionMode != "yolo" {
		t.Errorf("PermissionMode = %q, want yolo", b.PermissionMode)
	}
}

// TestCmdPermission_DirectArgRejectsInvalid covers the /perm <mode> direct
// path, which has rejected illegal values since v1; this locks the behaviour so
// the picker-path addition does not drift the two apart.
func TestCmdPermission_DirectArgRejectsInvalid(t *testing.T) {
	h, _ := newTestHandler(t)
	res, err := h.cmdPermission(context.Background(), "c1", []string{"ask"})
	if err == nil {
		t.Error("expected error for illegal approval mode via direct arg")
	}
	if !strings.Contains(res.Body, "未知审批模式") && (err != nil && !strings.Contains(err.Error(), "未知审批模式")) {
		t.Errorf("expected 未知审批模式 in body=%q err=%v", res.Body, err)
	}
}

// --- /current ---

// TestCmdCurrent_ShowsDefaults verifies /current reflects the config defaults
// (approval=write, thinking=auto) when no pin is set, and lazily creates the
// binding.
func TestCmdCurrent_ShowsDefaults(t *testing.T) {
	h, _ := newTestHandler(t)
	res, err := h.cmdCurrent(context.Background(), "c1", nil)
	if err != nil {
		t.Fatalf("cmdCurrent: %v", err)
	}
	for _, want := range []string{"审批模式", "思考级别", "write", "auto"} {
		if !strings.Contains(res.Body, want) {
			t.Errorf("body = %q, want contains %q", res.Body, want)
		}
	}
}

// --- /session-new, /session-del ---

// TestCmdSessionNew_NoBinding verifies the empty-state copy: a chat with no
// binding is told to just send a message.
func TestCmdSessionNew_NoBinding(t *testing.T) {
	h, _ := newTestHandler(t)
	res, err := h.cmdSessionNew(context.Background(), "c1", nil)
	if err != nil {
		t.Fatalf("cmdSessionNew: %v", err)
	}
	if !strings.Contains(res.Body, "尚无会话") {
		t.Errorf("body = %q, want contains '尚无会话'", res.Body)
	}
}

// TestCmdSessionDel_Unbinds verifies /session-del removes an existing binding.
func TestCmdSessionDel_Unbinds(t *testing.T) {
	h, r := newTestHandler(t)
	r.Bind("c1", "sess-1", "/tmp/proj", "", "glm-5.2", "")
	res, err := h.cmdSessionDel(context.Background(), "c1", nil)
	if err != nil {
		t.Fatalf("cmdSessionDel: %v", err)
	}
	if !strings.Contains(res.Body, "已删除") {
		t.Errorf("body = %q, want contains '已删除'", res.Body)
	}
	if _, ok := r.Lookup("c1"); ok {
		t.Error("binding still present after /session-del")
	}
}

// --- /model ---

// TestCmdModel_DirectPin verifies /model <spec> pins the spec and reports the
// before→after change.
func TestCmdModel_DirectPin(t *testing.T) {
	h, r := newTestHandler(t)
	res, err := h.cmdModel(context.Background(), "c1", []string{"glm-5.2"})
	if err != nil {
		t.Fatalf("cmdModel: %v", err)
	}
	if res.After != "glm-5.2" {
		t.Errorf("After = %q, want glm-5.2", res.After)
	}
	if b, _ := r.Lookup("c1"); b.ModelSpec != "glm-5.2" {
		t.Errorf("ModelSpec = %q, want glm-5.2", b.ModelSpec)
	}
}

// TestCmdModel_Clear verifies /model clear resets the pin.
func TestCmdModel_Clear(t *testing.T) {
	h, r := newTestHandler(t)
	r.Bind("c1", "", "/tmp/proj", "", "glm-5.2", "")
	res, err := h.cmdModel(context.Background(), "c1", []string{"clear"})
	if err != nil {
		t.Fatalf("cmdModel clear: %v", err)
	}
	if b, _ := r.Lookup("c1"); b.ModelSpec != "" {
		t.Errorf("ModelSpec = %q, want empty after clear", b.ModelSpec)
	}
	if !strings.Contains(res.Body, "清除") && !strings.Contains(res.Body, "默认") {
		t.Errorf("body = %q, want contains 清除/默认", res.Body)
	}
}

// --- /thinking (MakeEnumPicker wiring smoke) ---

// TestCmdThinking_DirectPin smoke-tests that omp's /thinking is wired to
// MakeEnumPicker with the omp thinking-level set (the picker-path validation is
// covered at the bridgebase level in enum_picker_test.go).
func TestCmdThinking_DirectPin(t *testing.T) {
	h, r := newTestHandler(t)
	res, err := h.cmdThinking(context.Background(), "c1", []string{"high"})
	if err != nil {
		t.Fatalf("cmdThinking: %v", err)
	}
	if b, _ := r.Lookup("c1"); b.EffortLevel != "high" {
		t.Errorf("EffortLevel = %q, want high", b.EffortLevel)
	}
	if res.After != "high" {
		t.Errorf("After = %q, want high", res.After)
	}
}

// TestCmdThinking_DirectArgRejectsInvalid verifies the direct path rejects a
// bogus level (MakeEnumPicker's Valid gate).
func TestCmdThinking_DirectArgRejectsInvalid(t *testing.T) {
	h, _ := newTestHandler(t)
	_, err := h.cmdThinking(context.Background(), "c1", []string{"ultra"})
	if err == nil {
		t.Error("expected error for illegal thinking level via direct arg")
	}
}
