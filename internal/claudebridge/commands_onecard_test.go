package claudebridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justphantom/claude-go-sdk"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/feishufront"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// newOneCardHandler wires a Handler whose emit goes through the real IPC
// client, so tests can assert the promptID/UpdateMessageID wiring of the
// picker flow against what the frontend would actually see. agent may be nil
// for the config-driven pickers (/model, /effort, /perm).
func newOneCardHandler(t *testing.T, agent claudeAPI, opts HandlerConfig) (*Handler, *router.Router, *feishufront.BackendRegistry) {
	t.Helper()
	client, reg, cleanup := connectTestRPC(t)
	t.Cleanup(cleanup)
	if opts.StateDir == "" {
		opts.StateDir = t.TempDir()
	}
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	h := NewWithLogger(r, agent, client, opts, log.Nop())
	return h, r, reg
}

// drainOfType drains controls until one of the given type arrives or the
// timeout elapses.
func drainOfType(t *testing.T, reg *feishufront.BackendRegistry, wantType string) *protocol.Control {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case rc := <-reg.Controls():
			if rc.Control.Type == wantType {
				return rc.Control
			}
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("no %s control arrived within timeout", wantType)
	return nil
}

// failingListClaude is a claudeAPI whose ListSettings always errors, to drive
// the /settings picker's pre-answer failure branch.
type failingListClaude struct{}

func (failingListClaude) Run(context.Context, claude.RunOptions) (<-chan claude.Event, error) {
	ch := make(chan claude.Event)
	close(ch)
	return ch, nil
}

func (failingListClaude) ListSettings(context.Context) ([]string, error) {
	return nil, errors.New("settings list unavailable")
}

// TestCmdModel_Picker_OneCardFlow pins the single-card contract for /model:
// the Question card carries the command message's promptID (so the frontend
// morphs its progress card into the picker), and the result Notice patches
// that same card via UpdateMessageID. Net one card end-to-end.
func TestCmdModel_Picker_OneCardFlow(t *testing.T) {
	h, _, reg := newOneCardHandler(t, nil, HandlerConfig{
		ModelOptions: []string{"haiku", "sonnet", "opus"},
	})

	// Dispatch blocks inside the picker until the answer arrives.
	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/model", "om_cmd")

	q := drainOfType(t, reg, protocol.TypeQuestion)
	if q.PromptID != "om_cmd" {
		t.Errorf("question promptID = %q, want om_cmd", q.PromptID)
	}
	if !q.Question.TakeOverProgress {
		t.Error("question should request progress-card takeover")
	}

	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1", MessageID: "om_progress", Choices: []string{"sonnet"},
	})

	res := drainOfType(t, reg, protocol.TypeNotice)
	if res.Notice.UpdateMessageID != "om_progress" {
		t.Errorf("result UpdateMessageID = %q, want om_progress", res.Notice.UpdateMessageID)
	}
}

// TestCmdModel_Picker_AnswerFailureOneCard verifies a picker failure (empty
// answer) terminates the command's progress card in place — the error Notice
// is bound to the command's promptID, not emitted as a standalone card that
// would leave the "处理中" placeholder hanging.
func TestCmdModel_Picker_AnswerFailureOneCard(t *testing.T) {
	h, _, reg := newOneCardHandler(t, nil, HandlerConfig{
		ModelOptions: []string{"haiku", "sonnet", "opus"},
	})

	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/model", "om_cmd")

	q := drainOfType(t, reg, protocol.TypeQuestion)
	// Empty answer: no choice, no custom → AskAndWait returns an error →
	// emitPromptNotice terminates the progress card.
	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1",
	})

	n := drainOfType(t, reg, protocol.TypeNotice)
	if n.PromptID != "om_cmd" {
		t.Errorf("error notice promptID = %q, want om_cmd", n.PromptID)
	}
	if n.Notice.Level != "error" {
		t.Errorf("notice level = %q, want error", n.Notice.Level)
	}
	if n.Notice.Title != "选择失败" {
		t.Errorf("notice title = %q, want 选择失败", n.Notice.Title)
	}
	if n.Notice.UpdateMessageID != "" {
		t.Errorf("failure notice must not carry UpdateMessageID, got %q", n.Notice.UpdateMessageID)
	}
}

// TestCmdSettings_Picker_PreAnswerFailureOneCard verifies a failure before
// the Question is sent (ListSettings errors) also terminates the progress
// card via emitPromptNotice, bound to the command's promptID.
func TestCmdSettings_Picker_PreAnswerFailureOneCard(t *testing.T) {
	h, _, reg := newOneCardHandler(t, failingListClaude{}, HandlerConfig{})

	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/settings", "om_cmd")

	n := drainOfType(t, reg, protocol.TypeNotice)
	if n.PromptID != "om_cmd" {
		t.Errorf("pre-answer failure notice promptID = %q, want om_cmd", n.PromptID)
	}
	if n.Notice.Level != "error" {
		t.Errorf("notice level = %q, want error", n.Notice.Level)
	}
	if n.Notice.Title != "选择失败" {
		t.Errorf("notice title = %q, want 选择失败", n.Notice.Title)
	}
}

// TestCmdDirectory_Picker_OneCardFlow pins the single-card contract for /cd:
// the Question card carries the command message's promptID (so the frontend
// morphs its progress card into the picker), and the result Notice patches
// that same card via UpdateMessageID. Net one card end-to-end.
func TestCmdDirectory_Picker_OneCardFlow(t *testing.T) {
	h, _, reg := newOneCardHandler(t, nil, HandlerConfig{})
	// /cd reads its options from DirCache.List, so seed a workspace with one
	// subdirectory the picker can offer.
	workspace := t.TempDir()
	subdir := filepath.Join(workspace, "proj-a")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	h.DirCache = bridgebase.NewDirCache(workspace)

	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/cd", "om_cmd")

	q := drainOfType(t, reg, protocol.TypeQuestion)
	if q.PromptID != "om_cmd" {
		t.Errorf("question promptID = %q, want om_cmd", q.PromptID)
	}
	if !q.Question.TakeOverProgress {
		t.Error("question should request progress-card takeover")
	}

	h.Answers.Deliver(q.Question.RequestID, &protocol.AnswerPayload{
		RequestID: q.Question.RequestID, ChatID: "chat-1", MessageID: "om_progress", Choices: []string{"proj-a"},
	})

	res := drainOfType(t, reg, protocol.TypeNotice)
	if res.Notice.UpdateMessageID != "om_progress" {
		t.Errorf("result UpdateMessageID = %q, want om_progress", res.Notice.UpdateMessageID)
	}
	if res.Notice.Level != "success" {
		t.Errorf("notice level = %q, want success", res.Notice.Level)
	}
	if res.Notice.Title != "已切换目录" {
		t.Errorf("notice title = %q, want 已切换目录", res.Notice.Title)
	}
}

// TestCmdDirectory_Picker_PreAnswerFailureOneCard verifies a failure before
// the Question is sent (DirCache.List errors on an unconfigured root)
// terminates the progress card via emitPromptNotice, bound to the command's
// promptID — no standalone card that would leave "处理中" hanging.
func TestCmdDirectory_Picker_PreAnswerFailureOneCard(t *testing.T) {
	// Empty WorkspaceRoot → DirCache.List errors ("未配置 WORKSPACE_ROOT").
	h, _, reg := newOneCardHandler(t, nil, HandlerConfig{})

	go commands.Dispatch(h, h.emit, h.Logger, context.Background(), "chat-1", "/cd", "om_cmd")

	n := drainOfType(t, reg, protocol.TypeNotice)
	if n.PromptID != "om_cmd" {
		t.Errorf("pre-answer failure notice promptID = %q, want om_cmd", n.PromptID)
	}
	if n.Notice.Level != "error" {
		t.Errorf("notice level = %q, want error", n.Notice.Level)
	}
	if n.Notice.Title != "选择失败" {
		t.Errorf("notice title = %q, want 选择失败", n.Notice.Title)
	}
	if n.Notice.UpdateMessageID != "" {
		t.Errorf("failure notice must not carry UpdateMessageID, got %q", n.Notice.UpdateMessageID)
	}
}
