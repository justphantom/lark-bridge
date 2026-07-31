package ompbridge

import (
	"context"
	"testing"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// ctxRecordAgent captures the ctx runPrompt hands to the subprocess run so a
// test can observe WHEN the scaffold is cancelled relative to the terminal
// emit (S3).
type ctxRecordAgent struct {
	ctxch chan context.Context
}

func (a *ctxRecordAgent) Run(ctx context.Context, _ omp.RunOptions) (<-chan omp.Event, error) {
	select {
	case a.ctxch <- ctx:
	default:
	}
	ch := make(chan omp.Event, 4)
	ch <- omp.Event{Type: omp.EventAgentStart}
	ch <- omp.Event{Type: omp.EventMessageUpdate, Text: "ok"}
	ch <- omp.Event{Type: omp.EventAgentEnd}
	close(ch)
	return ch, nil
}

func (a *ctxRecordAgent) ListModels(context.Context) ([]string, error) { return nil, nil }
func (a *ctxRecordAgent) ListSessions(context.Context, string) ([]omp.Session, error) {
	return nil, nil
}
func (a *ctxRecordAgent) DeleteSession(context.Context, string, string) error { return nil }
func (a *ctxRecordAgent) CleanSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (a *ctxRecordAgent) RunGC(context.Context, omp.GCOptions) (omp.GCResult, error) {
	return omp.GCResult{}, nil
}
func (a *ctxRecordAgent) IsReady(context.Context) error { return nil }

// TestRunPrompt_ScaffoldCancelledBeforeTerminal (S3): after the stream ends,
// runPrompt cancels the scaffold ctx (SIGKILLing any lingering CLI process
// group) BEFORE EmitTerminal can block on a slow IPC ACK budget. Observed
// via the ctx the agent's Run received: by the time the terminal control is
// readable from the registry, that ctx must already be Done.
func TestRunPrompt_ScaffoldCancelledBeforeTerminal(t *testing.T) {
	client, reg, cleanup := connectOmpTestRPC(t)
	defer cleanup()

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router new: %v", err)
	}
	agent := &ctxRecordAgent{ctxch: make(chan context.Context, 1)}
	h := NewWithLogger(r, agent, client, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{StateDir: t.TempDir()},
	}, log.Nop())
	t.Cleanup(h.Close)
	r.Bind("c1", "", t.TempDir(), "", "", "")

	if err := h.HandleEvent(context.Background(), &protocol.Event{
		Type:     protocol.TypePrompt,
		PromptID: "msg-s3",
		Prompt:   &protocol.PromptPayload{ChatID: "c1", Text: "hi"},
	}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}

	controls := drainOmpUntilTerminal(t, reg)
	if findControl(controls, protocol.TypeResult) == nil {
		t.Fatalf("no TypeResult; got %v", ompControlTypes(controls))
	}

	var runCtx context.Context
	select {
	case runCtx = <-agent.ctxch:
	default:
		t.Fatal("agent.Run was never called")
	}
	if runCtx.Err() == nil {
		t.Error("scaffold ctx not cancelled by terminal-emit time (S3 regression): " +
			"the CLI process group would linger as a zombie across a slow IPC emit")
	}
}
