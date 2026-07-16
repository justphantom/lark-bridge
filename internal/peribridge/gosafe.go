package peribridge

import (
	"runtime/debug"

	"github.com/hu/lark-bridge/internal/log"
)

// goSafe runs fn in a new goroutine, recovering from any panic and logging it
// via logger so the process keeps running. name is a short label for triage.
//
// Use goSafe for goroutines whose panic would otherwise crash the process or
// leak silently (dispatchCommand, processing-card sends, promptAsync). For
// goroutines that must signal a caller via a channel even on panic, do NOT
// use goSafe — write a dedicated defer recover that fills the channel with an
// error instead (the runPrompt path does this).
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
