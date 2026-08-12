package bridgebase

import (
	"context"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// These tests cover the terminal-delivery reliability layer:
//   - terminalRetryBackoff: the retry sleep ladder.
//   - EmitTerminalControl: the shared pkg-level retry loop (used by miniagent
//     and any backend that emits terminal controls directly).

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

// TestEmitTerminalControl_NilRPCNoop verifies a nil RPC (unit test / no
// frontend) returns immediately as a no-op success — it must NOT enter the
// retry loop or block.
func TestEmitTerminalControl_NilRPCNoop(t *testing.T) {
	appCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	err := EmitTerminalControl(log.Nop(), nil, appCtx, "p1", "c1",
		&protocol.Control{Type: protocol.TypeResult})
	elapsed := time.Since(start)
	if err != nil {
		t.Errorf("EmitTerminalControl = %v, want nil (nil RPC → no-op success)", err)
	}
	if elapsed > time.Second {
		t.Errorf("EmitTerminalControl took %v with nil RPC, want <1s (no-op)", elapsed)
	}
}
