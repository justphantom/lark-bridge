// Package bridgebase holds the pieces every backend bridge (claude, opencode)
// shares: the panic-safe goroutine launcher, the interactive-card answer
// broker, and the <think>-block stripper. Each bridge used to carry its own
// byte-for-byte copy of these; they live here now so a fix applies once and
// the three bridges cannot drift apart.
package bridgebase

import (
	"runtime/debug"

	"github.com/justphantom/lark-bridge/internal/log"
)

// GoSafe runs fn in a new goroutine, recovering from any panic and logging it
// via logger so the process keeps running. name is a short label used in the
// log line for triage.
//
// Use GoSafe for any goroutine whose panic would otherwise crash the process
// or leak silently (dispatchCommand, processing-card sends, poll-loop ticks,
// promptAsync). For goroutines that must signal a caller via a channel even on
// panic, do NOT use GoSafe — write a dedicated defer recover that fills the
// channel with an error instead (the runPrompt path already does this and
// should keep its own recover for that reason).
func GoSafe(logger *log.Logger, name string, fn func()) {
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
