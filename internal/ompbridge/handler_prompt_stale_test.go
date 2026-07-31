package ompbridge

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
)

// retryAgent drives the stale-session recovery orchestration in runPrompt.
// On the stale case its first Run yields the synthesised "Session … not
// found" EventError (the omp/17.1.8 bad-–resume signature the client pump
// produces), then its second Run yields a normal success stream. On the
// non-stale case every Run yields an unrelated error. runCount and
// secondSessionID let the test assert the retry fired exactly once AND that
// runPrompt cleared the binding's session id before retrying (the point the
// matcher-only test did not cover).
type retryAgent struct {
	stale           bool
	runCount        atomic.Int32
	secondSessionID string
}

func (a *retryAgent) Run(_ context.Context, opts omp.RunOptions) (<-chan omp.Event, error) {
	calls := a.runCount.Add(1)
	if calls == 2 {
		a.secondSessionID = opts.SessionID
	}
	ch := make(chan omp.Event, 4)
	switch {
	case a.stale && calls == 1:
		ch <- omp.Event{Type: omp.EventError, Text: `Error: Session "stale-id" not found.`}
	case a.stale: // 2nd call: success
		ch <- omp.Event{Type: omp.EventAgentStart}
		ch <- omp.Event{Type: omp.EventMessageUpdate, Text: "ok"}
		ch <- omp.Event{Type: omp.EventAgentEnd}
	default: // non-stale error every call
		ch <- omp.Event{Type: omp.EventError, Text: "rate limit exceeded"}
	}
	close(ch)
	return ch, nil
}

func (a *retryAgent) ListModels(context.Context) ([]string, error)                { return nil, nil }
func (a *retryAgent) ListSessions(context.Context, string) ([]omp.Session, error) { return nil, nil }
func (a *retryAgent) DeleteSession(context.Context, string, string) error         { return nil }
func (a *retryAgent) CleanSessions(context.Context, string, string) ([]string, error) {
	return nil, nil
}
func (a *retryAgent) RunGC(context.Context, omp.GCOptions) (omp.GCResult, error) {
	return omp.GCResult{}, nil
}
func (a *retryAgent) IsReady(context.Context) error { return nil }

// TestRunPrompt_StaleSessionRetriesOnce drives the real HandleEvent → runPrompt
// path and asserts the stale-session orchestration (handler_prompt.go:74-83):
// a stale --resume error is detected, the binding's sessionID is cleared, and
// exactly ONE retry with an empty session lands a success result. A non-stale
// error (rate limit) does NOT trigger a retry. Only the isStaleSessionErr
// matcher was previously covered; the orchestration had zero coverage.
func TestRunPrompt_StaleSessionRetriesOnce(t *testing.T) {
	cases := []struct {
		name           string
		stale          bool
		wantRuns       int32
		want2ndSession string // sessionID the 2nd Run saw ("" = cleared before retry)
		wantTerminal   string
	}{
		{"stale then success", true, 2, "", protocol.TypeResult},
		{"non-stale error no retry", false, 1, "", protocol.TypeError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, reg, cleanup := connectOmpTestRPC(t)
			defer cleanup()

			r, err := router.New("", log.Nop())
			if err != nil {
				t.Fatalf("router new: %v", err)
			}
			agent := &retryAgent{stale: tc.stale}
			h := NewWithLogger(r, agent, client, HandlerConfig{
				CoreConfig: bridgebase.CoreConfig{StateDir: t.TempDir()},
			}, log.Nop())
			t.Cleanup(h.Close)
			// Pre-existing binding carries a session id — the retry guard's
			// `binding.SessionID != ""` condition must hold for the stale case.
			r.Bind("c1", "stale-id", t.TempDir(), "", "", "")

			if err := h.HandleEvent(context.Background(), &protocol.Event{
				Type:     protocol.TypePrompt,
				PromptID: "msg-1",
				Prompt:   &protocol.PromptPayload{ChatID: "c1", Text: "hi"},
			}); err != nil {
				t.Fatalf("HandleEvent: %v", err)
			}

			controls := drainOmpUntilTerminal(t, reg)

			if got := agent.runCount.Load(); got != tc.wantRuns {
				t.Errorf("agent.Run calls = %d, want %d", got, tc.wantRuns)
			}
			if tc.stale && agent.secondSessionID != tc.want2ndSession {
				t.Errorf("2nd Run sessionID = %q, want %q (should be cleared before retry)",
					agent.secondSessionID, tc.want2ndSession)
			}
			term := findControl(controls, tc.wantTerminal)
			if term == nil {
				t.Errorf("no %s terminal emitted; got %v", tc.wantTerminal, ompControlTypes(controls))
			}
			// Stale case must not ALSO leave a stale TypeError from the first attempt.
			if tc.stale && findControl(controls, protocol.TypeError) != nil {
				t.Errorf("stale attempt's error leaked as a terminal TypeError; got %v",
					ompControlTypes(controls))
			}
		})
	}
}
