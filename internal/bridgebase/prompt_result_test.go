package bridgebase

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/router"
	"github.com/justphantom/lark-bridge/internal/usage"
)

// captureRPC is a minimal Core RPC substitute that records every control
// emitted, so RecordUsage / EmitTerminal tests can assert on the terminal
// control without an HTTP round-trip.
type captureRPC struct {
	controls []*protocol.Control
}

func (c *captureRPC) SendControl(_ context.Context, ctrl *protocol.Control) error {
	c.controls = append(c.controls, ctrl)
	return nil
}

func newResultCore(t *testing.T) (*Core, *usage.Store, *captureRPC) {
	t.Helper()
	rpc := &captureRPC{}
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	core := NewCore(r, nil, CoreConfig{}, log.Nop())
	core.RPC = nil // bypass real send
	// Inject a usage store that we can read back.
	dir := t.TempDir()
	store, err := usage.New(dir+"/u.json", log.Nop(), time.Hour)
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	core.Usage = store
	// Replace Emit so it routes through our rpc.
	return core, store, rpc
}

// TestRecordUsage_SkipsCancelled verifies a cancelled turn does not record
// (matching the contract that the SIGKILLed subprocess's terminal event was
// likely lost).
func TestRecordUsage_SkipsCancelled(t *testing.T) {
	core, store, _ := newResultCore(t)
	core.RecordUsage("chat", PromptResult{IsCancelled: true, SessionID: "s", InputTokens: 50})
	if e, ok := store.Get("s"); ok && (e.Input != 0 || e.Output != 0) {
		t.Fatalf("cancelled recorded: %+v", e)
	}
}

// TestRecordUsage_CacheCreationMapsToCacheWrite verifies the claude-style
// CacheCreation field lands in usage.Delta.CacheWrite (single dimension).
func TestRecordUsage_CacheCreationMapsToCacheWrite(t *testing.T) {
	core, store, _ := newResultCore(t)
	core.RecordUsage("chat", PromptResult{SessionID: "s1", InputTokens: 10, CacheCreation: 99})
	e, ok := store.Get("s1")
	if !ok {
		t.Fatal("missing session")
	}
	if e.CacheWrite != 99 {
		t.Errorf("CacheWrite = %d, want 99 (claude cacheCreation collapsed)", e.CacheWrite)
	}
}

// TestRecordUsage_CacheWriteWinsWhenBothSet verifies the explicit CacheWrite
// wins when both fields happen to be set (defensive — only one is set per
// backend today).
func TestRecordUsage_CacheWriteWinsWhenBothSet(t *testing.T) {
	core, store, _ := newResultCore(t)
	core.RecordUsage("chat", PromptResult{SessionID: "s1", CacheWrite: 7, CacheCreation: 99})
	e, ok := store.Get("s1")
	if !ok {
		t.Fatal("missing")
	}
	if e.CacheWrite != 7 {
		t.Errorf("CacheWrite = %d, want 7 (explicit CacheWrite wins)", e.CacheWrite)
	}
}

// TestBuildTerminalControl_CancelledWithReason (S2): a backend-initiated
// abort carrying a specific Err (e.g. omp's auto-retry limit) renders ONE
// terminal notice whose message is the reason, not the generic cancel copy.
func TestBuildTerminalControl_CancelledWithReason(t *testing.T) {
	core, _, _ := newResultCore(t)
	ctrl := core.buildTerminalControl(context.Background(), "c1", "omp", 0, PromptResult{
		IsCancelled: true,
		Err:         errors.New("自动重试已达上限（3次），终止回合"),
	})
	if ctrl.Type != protocol.TypeNotice || ctrl.Notice == nil {
		t.Fatalf("type = %v, want TypeNotice", ctrl.Type)
	}
	if ctrl.Notice.Message != "自动重试已达上限（3次），终止回合" {
		t.Errorf("message = %q, want the abort reason", ctrl.Notice.Message)
	}
}

// TestBuildTerminalControl_CancelledCtxErrKeepsGenericCopy: a plain ctx
// error (user abort / prompt timeout) must NOT leak "context canceled"
// into the user-facing notice — the generic copy stays.
func TestBuildTerminalControl_CancelledCtxErrKeepsGenericCopy(t *testing.T) {
	core, _, _ := newResultCore(t)
	ctrl := core.buildTerminalControl(context.Background(), "c1", "omp", 0, PromptResult{
		IsCancelled: true,
		Err:         context.Canceled,
	})
	if ctrl.Notice == nil || ctrl.Notice.Message != "本次请求已中止" {
		t.Errorf("message = %q, want generic cancel copy", ctrl.Notice.Message)
	}
}
