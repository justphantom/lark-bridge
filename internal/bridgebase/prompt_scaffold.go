package bridgebase

import (
	"context"
	"errors"
	"runtime/debug"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// PromptScaffold bundles the per-prompt context assembled by
// RunPromptScaffold, an onActivity hook (called once per received stdout
// event so the idle watchdog timer can be reset), a stop func that releases
// the watchdog timer, and the CancelCause func so the caller can fire the
// cancel with a cause (e.g. errIdleTimeout). The PromptTimeout timer is
// wired internally; callers only need to `defer scaffold.Stop()` and
// `defer scaffold.Cancel(nil)`.
type PromptScaffold struct {
	Ctx        context.Context
	Cancel     context.CancelCauseFunc
	OnActivity func()
	Stop       func()
}

// RunPromptScaffold assembles the per-prompt prologue shared by every CLI
// bridge's runPrompt: WithCancelCause parent, PromptTimeout timer, and the
// optional idle watchdog.
//
// Backends keep their panic-recover and the LIFO defer chain (Wg.Done /
// EndPrompt / mine.Cancel) in their own runPrompt — the scaffold only
// centralises the ctx+timer plumbing that was byte-identical across
// claude/opencode/omp. The divergent middle logic (opts construction, stale
// retry, streamRun dispatch, RecordUsage / EmitTerminal) stays local.
//
// idleTimeout > 0 wires the idle watchdog: each OnActivity() call resets the
// timer; when no event arrives for idleTimeout the timer fires
// Cancel(idleCause). A backend with no idle watchdog passes idleTimeout=0
// and idleCause=nil — OnActivity becomes a no-op and Stop is a no-op.
func (c *Core) RunPromptScaffold(
	parent context.Context,
	idleTimeout time.Duration,
	idleCause error,
) PromptScaffold {
	ctx, cancel := context.WithCancelCause(parent)

	// Capture cancel into a stable local the timer closures read; if we
	// reassigned `cancel` itself, the AfterFunc goroutine would race the
	// parent goroutine's reassignment.
	localCancel := cancel

	// stopOnce collects every timer's Stop into a single idempotent call so
	// the caller's defer scaffold.Stop() releases all timers regardless of
	// which fired.
	var stopOnce sync.Once
	var stops []func() bool

	// PromptTimeout: the per-prompt total wall-clock safety net. 0 disables
	// it (the CLI exits on its own). When >0 a prompt exceeding this duration
	// is cancelled with context.DeadlineExceeded so EmitTerminal shows
	// "请求超时" instead of the generic "已取消".
	if c.PromptTimeout > 0 {
		ptTimer := time.AfterFunc(c.PromptTimeout, func() {
			localCancel(context.DeadlineExceeded)
		})
		stops = append(stops, ptTimer.Stop)
	}

	// Idle watchdog (opencode/omp only; claude passes idleTimeout=0).
	var idleTimer *time.Timer
	if idleTimeout > 0 && idleCause != nil {
		idleTimer = time.AfterFunc(idleTimeout, func() {
			localCancel(idleCause)
		})
		stops = append(stops, idleTimer.Stop)
	}
	onActivity := func() {
		if idleTimer != nil {
			idleTimer.Reset(idleTimeout)
		}
	}
	stop := func() {
		stopOnce.Do(func() {
			for _, s := range stops {
				s()
			}
		})
	}

	return PromptScaffold{
		Ctx:        ctx,
		Cancel:     cancel,
		OnActivity: onActivity,
		Stop:       stop,
	}
}

// IsPromptTimeout reports whether ctx was cancelled by the PromptTimeout
// timer (vs a user /abort). Backends use this in their terminal emit
// path to pick the right copy ("请求超时" vs "已取消").
func IsPromptTimeout(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), context.DeadlineExceeded)
}

// RecoverPromptPanic is the panic recover shared by every CLI bridge's
// runPrompt goroutine. It logs the panic with a stack and emits a fallback
// error notice bound to replyToID (gated on AppCtx, not the cancelled parent
// ctx). Callers invoke it as
//
//	defer func() { RecoverPromptPanic(c, chatID, replyToID, recover()) }()
//
// so the recover() fires on the goroutine's exit path.
func RecoverPromptPanic(c *Core, chatID, replyToID string, r any) {
	if r == nil {
		return
	}
	c.Logger.Error("panic in runPrompt",
		log.FieldChatID, chatID,
		log.FieldPanic, r,
		log.FieldStack, debug.Stack())
	// Gate on appCtx, not parent: parent is cancelled by the caller's
	// mine.Cancel() defer so reading it here would always see "cancelled".
	if c.AppCtx.Err() == nil {
		c.EmitLogged(context.Background(), replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "error", Title: "内部错误", Message: "⚠️ 内部错误，已恢复"},
		})
	}
}
