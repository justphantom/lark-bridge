package bridgebase

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// capturingEmit is a test EmitFunc that records every Control it receives.
// Returns nil so callers exercise the success path.
func capturingEmit(out *[]*protocol.Control, mu *sync.Mutex) EmitFunc {
	return func(_ context.Context, _ string, ctrl *protocol.Control) error {
		mu.Lock()
		*out = append(*out, ctrl)
		mu.Unlock()
		return nil
	}
}

// newCmdCommands builds a Commands table with the given specs, generic over
// a trivial int handler type so the test does not need a real bridge.
func newCmdCommands(specs []CommandSpec[int]) *Commands[int] {
	return NewCommands[int](specs)
}

// TestDispatch_HappyPath verifies a successful handler's Body/Title/Level
// land in the emitted Notice.
func TestDispatch_HappyPath(t *testing.T) {
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/ping", Title: "Ping", Level: "success"},
		Handler: func(_ int, _ context.Context, _ string, _ []string) (cmdutil.Result, error) {
			return cmdutil.Result{Body: "pong"}, nil
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var got []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/ping", "")
	if len(got) != 1 {
		t.Fatalf("emits=%d, want 1", len(got))
	}
	n := got[0].Notice
	if n.Level != "success" || n.Title != "Ping" || n.Message != "pong" {
		t.Errorf("notice = %+v, want success/Ping/pong", n)
	}
	if got[0].ChatID != "C" {
		t.Errorf("chatID=%q, want C", got[0].ChatID)
	}
}

// TestDispatch_UnknownCommand verifies an unrecognized /xxx emits a warning
// notice with the help body so the user can self-correct.
func TestDispatch_UnknownCommand(t *testing.T) {
	cmds := newCmdCommands(nil)
	var mu sync.Mutex
	var got []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/wat", "")
	if len(got) != 1 {
		t.Fatalf("emits=%d, want 1", len(got))
	}
	n := got[0].Notice
	if n.Level != "warning" {
		t.Errorf("level=%q, want warning", n.Level)
	}
	if !strings.Contains(n.Message, "未知命令") || !strings.Contains(n.Message, "/wat") {
		t.Errorf("body=%q, want it to mention the unknown command", n.Message)
	}
}

// TestDispatch_HandlerError verifies a handler returning an error surfaces
// it as an error-level notice with the message prefixed by ⚠️.
func TestDispatch_HandlerError(t *testing.T) {
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/boom", Title: "Boom"},
		Handler: func(_ int, _ context.Context, _ string, _ []string) (cmdutil.Result, error) {
			return cmdutil.Result{Body: "ignored"}, errors.New("disk full")
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var got []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/boom", "")
	if len(got) != 1 {
		t.Fatalf("emits=%d, want 1", len(got))
	}
	n := got[0].Notice
	if n.Level != "error" {
		t.Errorf("level=%q, want error", n.Level)
	}
	if !strings.Contains(n.Message, "disk full") {
		t.Errorf("body=%q, want it to carry the error message", n.Message)
	}
}

// TestDispatch_StampsReplyToID verifies Dispatch carries the triggering
// message's ID on the handler ctx so picker handlers can target the progress
// card the frontend opened for that message.
func TestDispatch_StampsReplyToID(t *testing.T) {
	var got string
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/pick", Title: "Pick"},
		Handler: func(_ int, ctx context.Context, _ string, _ []string) (cmdutil.Result, error) {
			got = ReplyToID(ctx)
			return cmdutil.Result{Handled: true}, nil
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var controls []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&controls, &mu), log.Nop(), context.Background(), "C", "/pick", "om_user_msg")
	if got != "om_user_msg" {
		t.Errorf("ReplyToID = %q, want om_user_msg", got)
	}
}

// TestReplyToID_OutsideDispatch verifies the zero value outside a Dispatch ctx.
func TestReplyToID_OutsideDispatch(t *testing.T) {
	if got := ReplyToID(context.Background()); got != "" {
		t.Errorf("ReplyToID = %q, want empty", got)
	}
}

// TestDispatch_HandledSkipsEmit verifies a handler that signals Handled
// (e.g. an interactive picker that emits its own card) does NOT get a
// default TypeNotice — that would clobber the picker's card.
func TestDispatch_HandledSkipsEmit(t *testing.T) {
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/pick", Title: "Pick"},
		Handler: func(_ int, _ context.Context, _ string, _ []string) (cmdutil.Result, error) {
			return cmdutil.Result{Body: "picker took over", Handled: true}, nil
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var got []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/pick", "")
	if len(got) != 0 {
		t.Errorf("Handled=true should suppress emit, got %d controls", len(got))
	}
}

// TestDispatch_HandledWithErrorOverrides verifies that even when Handled is
// set, a returned error still triggers a notice — failures from a
// self-handling command must not be silently swallowed.
func TestDispatch_HandledWithErrorOverrides(t *testing.T) {
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/pick", Title: "Pick"},
		Handler: func(_ int, _ context.Context, _ string, _ []string) (cmdutil.Result, error) {
			return cmdutil.Result{Handled: true}, errors.New("picker crashed")
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var got []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/pick", "")
	if len(got) != 1 {
		t.Fatalf("emits=%d, want 1 (error overrides Handled)", len(got))
	}
	if got[0].Notice.Level != "error" || !strings.Contains(got[0].Notice.Message, "picker crashed") {
		t.Errorf("notice=%+v, want error carrying the message", got[0].Notice)
	}
}

// TestDispatch_Timeout verifies a handler that blocks past cmdutil.Timeout
// surfaces a timeout-warning notice rather than hanging the caller. The
// dispatcher's ctx gets a deadline from cmdutil.Timeout. Skipped under
// -short because cmdutil.Timeout is 15s (the real production bound).
func TestDispatch_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("cmdutil.Timeout is 15s; skip under -short")
	}
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/slow", Title: "Slow"},
		Handler: func(_ int, ctx context.Context, _ string, _ []string) (cmdutil.Result, error) {
			// Block until the dispatcher's timeout ctx fires.
			<-ctx.Done()
			return cmdutil.Result{}, ctx.Err()
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var got []*protocol.Control
	start := time.Now()
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/slow", "")
	elapsed := time.Since(start)
	if len(got) != 1 {
		t.Fatalf("emits=%d, want 1", len(got))
	}
	n := got[0].Notice
	if n.Level != "warning" || !strings.Contains(n.Message, "超时") {
		t.Errorf("notice=%+v, want warning mentioning timeout", n)
	}
	if elapsed > 20*time.Second {
		t.Errorf("dispatch took %s, should be bounded near cmdutil.Timeout", elapsed)
	}
}

// TestDispatch_ChangeResultFields verifies a ChangeResult's Field/Before/After
// are propagated into the NoticePayload so the renderer draws a before→after
// block, not just the body.
func TestDispatch_ChangeResultFields(t *testing.T) {
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/set", Title: "Set", Level: "success"},
		Handler: func(_ int, _ context.Context, _ string, _ []string) (cmdutil.Result, error) {
			return cmdutil.ChangeResult("模型", "old", "new", "下次生效"), nil
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var got []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/set", "")
	if len(got) != 1 {
		t.Fatalf("emits=%d, want 1", len(got))
	}
	n := got[0].Notice
	if n.Field != "模型" || n.Before != "old" || n.After != "new" {
		t.Errorf("change fields lost: %+v", n)
	}
	if n.Message != "下次生效" {
		t.Errorf("body=%q, want 下次生效", n.Message)
	}
}

// TestDispatch_PassesArgs verifies args after the command name are parsed
// and forwarded to the handler in order.
func TestDispatch_PassesArgs(t *testing.T) {
	var seenArgs []string
	var seenArgCount int64
	specs := []CommandSpec[int]{{
		Spec: cmdutil.Spec{Name: "/echo", Title: "Echo"},
		Handler: func(_ int, _ context.Context, _ string, args []string) (cmdutil.Result, error) {
			seenArgs = args
			atomic.AddInt64(&seenArgCount, 1)
			return cmdutil.Result{Body: strings.Join(args, " ")}, nil
		},
	}}
	cmds := newCmdCommands(specs)
	var mu sync.Mutex
	var got []*protocol.Control
	cmds.Dispatch(0, capturingEmit(&got, &mu), log.Nop(), context.Background(), "C", "/echo hello world", "")
	if atomic.LoadInt64(&seenArgCount) != 1 {
		t.Fatalf("handler not called once")
	}
	if len(seenArgs) != 2 || seenArgs[0] != "hello" || seenArgs[1] != "world" {
		t.Errorf("args=%v, want [hello world]", seenArgs)
	}
	if len(got) != 1 || got[0].Notice.Message != "hello world" {
		t.Errorf("body=%q, want 'hello world'", got[0].Notice.Message)
	}
}

// TestRenderHelp verifies the help body lists each spec on its own line
// with name and summary.
func TestRenderHelp(t *testing.T) {
	specs := []CommandSpec[int]{
		{Spec: cmdutil.Spec{Name: "/a", Summary: "alpha", Args: "[x]"}},
		{Spec: cmdutil.Spec{Name: "/b", Summary: "beta"}},
	}
	cmds := newCmdCommands(specs)
	help := cmds.RenderHelp()
	for _, want := range []string{"/a", "[x]", "alpha", "/b", "beta"} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q\ngot: %s", want, help)
		}
	}
}
