package bridgebase

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWaitFor_ResolveBeforeTimeout is the happy path: an ACK resolves the
// wait with nil (delivered).
func TestWaitFor_ResolveBeforeTimeout(t *testing.T) {
	r := NewAckRegistry(nil)
	done := make(chan error, 1)
	go func() { done <- r.WaitFor("p1", 5*time.Second) }()
	// Let WaitFor arm before resolving.
	time.Sleep(20 * time.Millisecond)
	r.Resolve("p1")
	if err := <-done; err != nil {
		t.Fatalf("WaitFor = %v, want nil on ACK", err)
	}
}

// TestWaitFor_CloseReturnsClosed locks in the low-10 fix: a wait drained by
// Close returns ErrAckRegistryClosed — NOT nil — so an in-flight terminal
// control during shutdown is never misread as delivered (which previously
// skipped retries and inflated the ACK-confirmed metric).
func TestWaitFor_CloseReturnsClosed(t *testing.T) {
	r := NewAckRegistry(nil)
	done := make(chan error, 1)
	go func() { done <- r.WaitFor("p2", time.Minute) }()
	time.Sleep(20 * time.Millisecond)
	r.Close()
	err := <-done
	if !errors.Is(err, ErrAckRegistryClosed) {
		t.Fatalf("WaitFor after Close = %v, want ErrAckRegistryClosed", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("Close must be distinguishable from a timeout")
	}
}

// TestAckRegistry_CloseIdempotent verifies a second Close is a no-op (no
// double-close panic) and that Resolve after Close stays a harmless no-op.
func TestAckRegistry_CloseIdempotent(t *testing.T) {
	r := NewAckRegistry(nil)
	r.Arm("p3")
	r.Close()
	r.Close() // must not panic
	r.Resolve("p3")
	// A wait armed AFTER Close must not block: done is already closed.
	if err := r.WaitFor("p4", time.Minute); !errors.Is(err, ErrAckRegistryClosed) {
		t.Fatalf("WaitFor armed post-Close = %v, want ErrAckRegistryClosed", err)
	}
}
