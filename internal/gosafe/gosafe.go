// Package gosafe provides Go, a goroutine launcher that recovers from panics
// so a stray bug in a long-lived goroutine cannot crash the process. It is the
// single home for the panic-safe-launch pattern previously duplicated in
// bridgebase and backendrpc.
package gosafe

import (
	"runtime/debug"

	"github.com/justphantom/lark-bridge/internal/log"
)

// Go runs fn in a new goroutine, recovering from any panic and logging it via
// logger so the process keeps running. name is a short label used in the log
// line for triage. A nil logger suppresses the log (the panic is still
// recovered, so a nil-logger caller degrades to the silent-swallow variant).
//
// Use Go for any goroutine whose panic would otherwise crash the process or
// leak silently (event loops, periodic sweepers, async sends). For goroutines
// that must signal a caller via a channel even on panic, do NOT use Go — write
// a dedicated defer recover that fills the channel with an error instead.
func Go(logger *log.Logger, name string, fn func()) {
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
