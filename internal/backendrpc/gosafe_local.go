package backendrpc

import (
	"runtime/debug"

	"github.com/justphantom/lark-bridge/internal/log"
)

// goSafe runs fn in a new goroutine, recovering from any panic and logging
// it so the process keeps running. It is a local copy of
// bridgebase.GoSafe — duplicated here because bridgebase already imports
// backendrpc (the IPC client the bridges share), so backendrpc cannot import
// bridgebase without creating an import cycle. Keep this in sync with
// bridgebase.GoSafe; the two are intentionally identical.
//
// Use goSafe for backendrpc's long-lived goroutines (readSSE, metrics loop)
// whose panic would otherwise crash the backend process.
func goSafe(logger *log.Logger, name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if logger != nil {
					logger.Error("panic in goroutine", log.FieldGoroutine, name, log.FieldPanic, r, log.FieldStack, string(debug.Stack()))
				}
			}
		}()
		fn()
	}()
}
