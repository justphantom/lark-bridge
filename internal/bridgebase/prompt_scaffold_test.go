package bridgebase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

func newScaffoldCore(t *testing.T) *Core {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	return NewCore(r, nil, CoreConfig{}, log.Nop())
}

// TestRunPromptScaffold_NoIdleWatchdog verifies idleTimeout=0 yields a
// no-op OnActivity / Stop.
func TestRunPromptScaffold_NoIdleWatchdog(t *testing.T) {
	c := newScaffoldCore(t)
	s := c.RunPromptScaffold(context.Background(), 0, nil)
	if s.OnActivity == nil {
		t.Fatal("OnActivity should never be nil (callers may invoke unconditionally)")
	}
	s.OnActivity() // must not panic
	s.Stop()
	s.Cancel(nil)
}

// TestRunPromptScaffold_IdleFiresCancelCause verifies an idle watchdog
// timeout cancels the ctx with the supplied cause (which streamRun reads to
// surface isIdleTimeout on the result).
func TestRunPromptScaffold_IdleFiresCancelCause(t *testing.T) {
	c := newScaffoldCore(t)
	cause := errors.New("test idle")
	s := c.RunPromptScaffold(context.Background(), 20*time.Millisecond, cause)
	defer s.Stop()

	select {
	case <-s.Ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("idle watchdog never fired")
	}
	if !errors.Is(context.Cause(s.Ctx), cause) {
		t.Errorf("cause = %v, want %v", context.Cause(s.Ctx), cause)
	}
}

// TestRunPromptScaffold_PromptTimeoutFires verifies the Core's PromptTimeout
// (total wall-clock) cancels the ctx with DeadlineExceeded.
func TestRunPromptScaffold_PromptTimeoutFires(t *testing.T) {
	r, _ := router.New("", log.Nop())
	c := NewCore(r, nil, CoreConfig{PromptTimeout: 30 * time.Millisecond}, log.Nop())
	s := c.RunPromptScaffold(context.Background(), 0, nil)
	defer s.Stop()

	select {
	case <-s.Ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("prompt timeout never fired")
	}
	if !IsPromptTimeout(s.Ctx) {
		t.Errorf("cause = %v, want DeadlineExceeded", context.Cause(s.Ctx))
	}
}
