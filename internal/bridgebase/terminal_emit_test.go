package bridgebase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// These tests cover the terminal-delivery reliability layer:
//   - AckRegistry: the one-shot wait the EmitTerminalControl retry loop arms,
//     paired with the frontend's ACK (see feishufront/dispatcher_ack_test.go
//     for the send side of the contract).
//   - terminalRetryBackoff: the retry sleep ladder.
//   - EmitTerminalControl: the shared pkg-level retry+ACK loop (used by
//     miniagent and any backend that emits terminal controls directly).

func TestAckRegistry_ResolveSignalsWait(t *testing.T) {
	ar := NewAckRegistry(log.Nop())
	go func() {
		time.Sleep(20 * time.Millisecond)
		ar.Resolve("p1")
	}()
	if err := ar.WaitFor("p1", time.Second); err != nil {
		t.Fatalf("WaitFor = %v, want nil (resolved before timeout)", err)
	}
	// The wait was consumed; a second Resolve is a no-op (late ACK).
	ar.Resolve("p1") // must not panic on double-close
}

func TestAckRegistry_TimeoutReturnsDeadlineExceeded(t *testing.T) {
	ar := NewAckRegistry(log.Nop())
	err := ar.WaitFor("p-noack", 30*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitFor = %v, want DeadlineExceeded", err)
	}
}

func TestAckRegistry_ForgetReapsEntry(t *testing.T) {
	ar := NewAckRegistry(log.Nop())
	ar.Arm("p-forget")
	ar.Forget("p-forget")
	// After Forget, Resolve must not panic (no pending channel to close).
	ar.Resolve("p-forget")
}

func TestAckRegistry_CloseUnblocksAllWaiters(t *testing.T) {
	ar := NewAckRegistry(log.Nop())
	ar.Arm("p-a")
	ar.Arm("p-b")
	done := make(chan struct{})
	go func() {
		_ = ar.WaitFor("p-a", 5*time.Second)
		close(done)
	}()
	time.Sleep(20 * time.Millisecond)
	ar.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock a pending WaitFor within 1s")
	}
}

func TestAckRegistry_IsAckEvent(t *testing.T) {
	ar := NewAckRegistry(log.Nop())
	if !ar.IsAckEvent(&protocol.Event{Type: protocol.TypeAck, PromptID: "p"}) {
		t.Error("IsAckEvent(TypeAck) = false, want true")
	}
	if ar.IsAckEvent(&protocol.Event{Type: protocol.TypePrompt}) {
		t.Error("IsAckEvent(TypePrompt) = true, want false")
	}
	if ar.IsAckEvent(nil) {
		t.Error("IsAckEvent(nil) = true, want false")
	}
}

func TestTerminalRetryBackoff(t *testing.T) {
	// Attempt 1 does not sleep (caller skips); 2→1s, 3→2s, 4+→4s cap.
	cases := map[int]time.Duration{
		2: 1 * time.Second,
		3: 2 * time.Second,
		4: 4 * time.Second,
		5: 4 * time.Second,
	}
	for attempt, want := range cases {
		if got := terminalRetryBackoff(attempt); got != want {
			t.Errorf("terminalRetryBackoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// TestEmitTerminalControl_NilRPCSkipsACKWait is the regression guard for the
// hung-wait bug: a call with a non-nil Acks registry but a nil RPC (unit test
// / no frontend) MUST NOT wait for an ACK that can never arrive —
// EmitTerminalControl returns success immediately (no-op), exiting on the
// first iteration instead of blocking ackWaitBudget.
func TestEmitTerminalControl_NilRPCSkipsACKWait(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	err := EmitTerminalControl(log.Nop(), nil, NewAckRegistry(log.Nop()), appCtx, "p1", "c1",
		&protocol.Control{Type: protocol.TypeResult})
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("EmitTerminalControl = %v, want nil (nil RPC → no-op success)", err)
	}
	// Must return near-instantly, NOT wait ackWaitBudget (6s). 1s is generous
	// for scheduler jitter while still catching a hung wait.
	if elapsed > time.Second {
		t.Errorf("EmitTerminalControl took %v with nil RPC, want <1s (must skip ACK wait)", elapsed)
	}
}
