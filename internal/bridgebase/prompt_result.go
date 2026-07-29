package bridgebase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/usage"
)

// CancelNoticeTimeout bounds the fresh context used to emit the "已取消"
// notice after the prompt ctx is already cancelled. Kept as a shared constant
// so every CLI bridge's EmitTerminal applies the same budget.
const CancelNoticeTimeout = 5 * time.Second

// messageIdleTimeout formats the per-backend idle-timeout message that
// matches the historical wording ("omp 已 N 秒无输出，已终止"). Kept as a
// helper so EmitTerminal stays a single switch.
func messageIdleTimeout(backendName string, idleTimeoutSec int) string {
	return fmt.Sprintf("%s 已 %d 秒无输出，已终止", backendName, idleTimeoutSec)
}

// PromptResult is the value a stream loop delivers once a CLI agent turn
// finishes (success, error, or cancellation). All three CLI bridges
// (claude/opencode/omp) had a near-identical local copy; promoting it lets
// RecordUsage / EmitTerminal live in the shared Core without a per-bridge
// adapter.
//
// CacheWrite vs CacheCreation: claude's stream-json calls the cache-creation
// token count `cache_creation_input_tokens`; opencode/omp call it `cacheWrite`
// (camelCase NDJSON). To keep one shape, both fields exist — claude fills
// CacheCreation, opencode/omp fill CacheWrite; RecordUsage collapses the
// difference (CacheWrite wins if non-zero, else CacheCreation).
type PromptResult struct {
	Reply        string
	Err          error
	Model        string
	SessionID    string
	DurationMs   int64
	ContextToken int // input+output (non-cache) shown on the result card
	CostUSD      float64
	Steps        int
	IsCancelled  bool
	// IsIdleTimeout is true when the turn was aborted by the idle watchdog.
	// Distinct from IsCancelled so EmitTerminal shows "响应超时" instead of
	// the generic "已取消". Currently only opencode/omp set this.
	IsIdleTimeout bool
	// Stale marks a "session no longer exists" error so runPrompt can drop the
	// binding's session id and retry once. Currently only claude sets this
	// (opencode does not have --resume; omp sets it via isStaleSessionErr at
	// runPrompt level, not on the result).
	Stale bool

	// Per-turn token breakdown fed to the usage store. CacheCreation and
	// CacheWrite are the same dimension under different CLI names; RecordUsage
	// collapses them into the usage.Delta's CacheWrite field.
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheWrite    int // opencode/omp name
	CacheCreation int // claude name; collapsed into CacheWrite by RecordUsage
}

// RecordUsage feeds the turn's token breakdown to the usage store. A
// cancelled turn is skipped: the subprocess was SIGKILLed and its terminal
// event (the source of these counts) typically did not arrive, so the numbers
// would be zero or stale. Errors are still recorded — a failed run that
// consumed tokens is real cost.
//
// Cache dimension collapse: callers that fill CacheCreation (claude) leave
// CacheWrite at 0; opencode/omp fill CacheWrite and leave CacheCreation at 0.
// Either way, the non-zero value lands in usage.Delta.CacheWrite.
func (c *Core) RecordUsage(chatID string, r PromptResult) {
	if c.Usage == nil || r.IsCancelled || r.SessionID == "" {
		return
	}
	cacheWrite := r.CacheWrite
	if cacheWrite == 0 {
		cacheWrite = r.CacheCreation
	}
	c.Usage.Add(usage.Delta{
		SessionID:  r.SessionID,
		ChatID:     chatID,
		Input:      r.InputTokens,
		Output:     r.OutputTokens,
		CacheRead:  r.CacheRead,
		CacheWrite: cacheWrite,
		Cost:       r.CostUSD,
		Turns:      1,
	})
}

// EmitTerminal renders the terminal control for a finished turn: cancelled
// → info notice, idle-timeout → warning notice, error → error control,
// success → result control. All branches use a fresh short-lived context
// (not the prompt ctx) so the terminal control still reaches the frontend
// when the prompt ctx is already cancelled (user abort, prompt timeout, or
// IPC blip during the turn).
//
// backendName personalises the cancel/timeout copy ("omp 已 N 秒无输出" vs
// "opencode 响应超时"), absorbing what was previously three ~40-line
// byte-similar functions.
//
// idleTimeoutSec is the configured IdleTimeout in seconds — passed in (rather
// than read from c.IdleTimeout) so backends without an idle watchdog can
// surface the right copy without wiring the field. Pass 0 if no idle timeout
// is configured; the idle branch is then unreachable (callers should also
// leave r.IsIdleTimeout false in that case).
func (c *Core) EmitTerminal(ctx context.Context, chatID, replyToID, backendName string, idleTimeoutSec int, r PromptResult) {
	sendCtx, cancel := context.WithTimeout(context.Background(), CancelNoticeTimeout)
	defer cancel()

	switch {
	case r.IsIdleTimeout:
		// Preserve the original per-backend wording that surfaced the
		// configured timeout so the user knows what budget was exceeded.
		msg := backendName + " 已无输出，已终止"
		if idleTimeoutSec > 0 {
			msg = messageIdleTimeout(backendName, idleTimeoutSec)
		}
		c.EmitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{
				Level:   "warning",
				Title:   "响应超时",
				Message: msg,
			},
		})
	case r.IsCancelled:
		title := "已取消"
		msg := "本次请求已中止"
		if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			title = "请求超时"
			msg = backendName + " 响应超时，已终止"
		}
		c.EmitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{Level: "info", Title: title, Message: msg},
		})
	case r.Err != nil:
		c.EmitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeError,
			ChatID: chatID,
			Error:  &protocol.ErrorPayload{Message: r.Err.Error()},
		})
	default:
		// Cumulative input+output across this session's turns (including this
		// one, already recorded by RecordUsage above). 0 when no store or no
		// history; the renderer hides the cumulative portion then.
		var totalTokens int
		if c.Usage != nil {
			if e, ok := c.Usage.Get(r.SessionID); ok {
				totalTokens = e.Input + e.Output
			}
		}
		c.EmitLogged(sendCtx, replyToID, chatID, &protocol.Control{
			Type:   protocol.TypeResult,
			ChatID: chatID,
			Result: &protocol.ResultPayload{
				Text:        r.Reply,
				Model:       r.Model,
				Tokens:      r.ContextToken,
				Duration:    time.Duration(r.DurationMs) * time.Millisecond,
				SessionID:   r.SessionID,
				Cost:        r.CostUSD,
				Steps:       r.Steps,
				TotalTokens: totalTokens,
			},
		})
	}
}
