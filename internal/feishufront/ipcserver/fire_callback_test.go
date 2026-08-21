package ipcserver

import (
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront"
)

// TestFireCallback_RecoversPanic verifies a panicking online/offline callback
// is recovered and logged, not propagated to crash the process.
func TestFireCallback_RecoversPanic(t *testing.T) {
	srv := NewIPCServer(feishufront.NewBackendRegistry(), "")
	boom := func(string, string) { panic("callback boom") }
	srv.onOffline.Store(&boom)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.fireCallback(srv.onOffline.Load(), "back-1", "claude", "offline")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fireCallback did not return after panic")
	}
}
