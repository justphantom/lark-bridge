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

// TestRunPrompt_NoIdleWatchdog verifies idleTimeout=0 yields a no-op
// onActivity callback and RunPrompt returns fn's result.
func TestRunPrompt_NoIdleWatchdog(t *testing.T) {
	c := newScaffoldCore(t)
	called := false
	err := c.RunPrompt(context.Background(), 0, nil, func(ctx context.Context, onActivity func()) error {
		called = true
		onActivity() // must not panic
		if ctx.Err() != nil {
			t.Error("ctx should not be cancelled when PromptTimeout is 0")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
	if !called {
		t.Fatal("RunPrompt did not invoke fn")
	}
}

// TestRunPrompt_IdleFiresCancelCause verifies an idle watchdog timeout
// cancels the ctx with the supplied cause (which streamRun reads to surface
// isIdleTimeout on the result).
func TestRunPrompt_IdleFiresCancelCause(t *testing.T) {
	c := newScaffoldCore(t)
	cause := errors.New("test idle")
	err := c.RunPrompt(context.Background(), 20*time.Millisecond, cause, func(ctx context.Context, onActivity func()) error {
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("idle watchdog never fired")
		}
		if !errors.Is(context.Cause(ctx), cause) {
			t.Errorf("cause = %v, want %v", context.Cause(ctx), cause)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
}

// TestRunPrompt_PromptTimeoutFires verifies the Core's PromptTimeout
// (total wall-clock) cancels the ctx with DeadlineExceeded.
func TestRunPrompt_PromptTimeoutFires(t *testing.T) {
	r, _ := router.New("", log.Nop())
	c := NewCore(r, nil, CoreConfig{PromptTimeout: 30 * time.Millisecond}, log.Nop())
	err := c.RunPrompt(context.Background(), 0, nil, func(ctx context.Context, onActivity func()) error {
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
			t.Fatal("prompt timeout never fired")
		}
		if !IsPromptTimeout(ctx) {
			t.Errorf("cause = %v, want DeadlineExceeded", context.Cause(ctx))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
}

// TestRunPrompt_IdleResetByActivity verifies onActivity resets the idle
// watchdog, allowing a long-running fn to outlive the idle interval as long
// as it stays active.
func TestRunPrompt_IdleResetByActivity(t *testing.T) {
	c := newScaffoldCore(t)
	cause := errors.New("test idle")
	err := c.RunPrompt(context.Background(), 40*time.Millisecond, cause, func(ctx context.Context, onActivity func()) error {
		for range 4 {
			select {
			case <-ctx.Done():
				t.Fatal("ctx cancelled before activity reset expired")
			case <-time.After(25 * time.Millisecond):
			}
			onActivity()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RunPrompt returned unexpected error: %v", err)
	}
}
