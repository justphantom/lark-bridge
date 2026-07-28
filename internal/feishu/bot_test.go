package feishu

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/justphantom/lark-bridge/internal/lark"
	"github.com/justphantom/lark-bridge/internal/log"
)

// withClientFactory routes NewBotWithLogger's client through a fake instead
// of constructing a real *lark.Client. Test-only BotOption; production never
// wires it.
func withClientFactory(c feishuClient) BotOption {
	return func(cfg *botConfig) { cfg.clientFactory = c }
}

// fakeClient is a controllable stand-in for *lark.Client. Each method
// records its call count and returns the configured result/error, letting
// tests assert the Bot's send/update paths without a real REST round-trip.
type fakeClient struct {
	sendResult *lark.SendResult
	sendErr    error
	patchErr   error
	// patchErrOnNth, when non-nil, returns the error only on the Nth call
	// (1-indexed); other calls use patchErr. Lets the fallback-card test
	// simulate "first patch rejected, second succeeds".
	patchErrOnNth    int32
	patchCalls       atomic.Int32
	patchLast        string
	startErr         error
	started          atomic.Int32
	stopped          atomic.Int32
	setHandlerCalled atomic.Bool
	// download result + call capture (file pipeline tests).
	downloadBody   string
	downloadErr    error
	downloadCalled atomic.Int32
}

func (f *fakeClient) Send(context.Context, *lark.SendInput) (*lark.SendResult, error) {
	return f.sendResult, f.sendErr
}
func (f *fakeClient) PatchMessage(_ context.Context, _, content string) error {
	n := f.patchCalls.Add(1)
	f.patchLast = content
	if f.patchErrOnNth != 0 && n == f.patchErrOnNth {
		return f.patchErr
	}
	if f.patchErrOnNth == 0 {
		return f.patchErr
	}
	return nil
}
func (f *fakeClient) DownloadResource(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
	f.downloadCalled.Add(1)
	if f.downloadErr != nil {
		return nil, f.downloadErr
	}
	return io.NopCloser(strings.NewReader(f.downloadBody)), nil
}
func (f *fakeClient) UploadFile(_ context.Context, _, _ string, _ io.Reader) (string, error) {
	return "fk_fake", nil
}
func (f *fakeClient) SetHandler(lark.Handler)     { f.setHandlerCalled.Store(true) }
func (f *fakeClient) SetLifecycle(lark.Lifecycle) {}
func (f *fakeClient) Start(context.Context) error { f.started.Add(1); return f.startErr }
func (f *fakeClient) Stop(context.Context) error  { f.stopped.Add(1); return nil }

// newFakeBot returns a Bot wired to a fakeClient, skipping the real lark
// client construction. Used by tests that exercise the Bot's own logic.
func newFakeBot(t *testing.T, fc *fakeClient) *Bot {
	t.Helper()
	b, err := NewBotWithLogger("app", "secret", log.Nop(), withClientFactory(fc))
	if err != nil {
		t.Fatalf("NewBotWithLogger: %v", err)
	}
	return b
}

// TestNewBotWithLogger_WiresHandlerAndLifecycle verifies the constructor
// installs the lark.Handler adapter and stores the client.
func TestNewBotWithLogger_WiresHandlerAndLifecycle(t *testing.T) {
	fc := &fakeClient{}
	b := newFakeBot(t, fc)
	if b.client == nil {
		t.Fatal("b.client = nil")
	}
	if !fc.setHandlerCalled.Load() {
		t.Fatal("SetHandler not called by constructor")
	}
}

// TestStart_Stop_Delegates verifies Start/Stop reach the underlying client.
func TestStart_Stop_Delegates(t *testing.T) {
	fc := &fakeClient{}
	b := newFakeBot(t, fc)
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := fc.started.Load(); got != 1 {
		t.Errorf("client.Start count = %d, want 1", got)
	}
	if err := b.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := fc.stopped.Load(); got != 1 {
		t.Errorf("client.Stop count = %d, want 1", got)
	}
}
