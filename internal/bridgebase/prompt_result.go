package bridgebase

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// EmitTerminalControl sends one terminal Control with a send-failure retry
// budget.
//
// The terminal control is the single most important message of a turn (the
// final reply / error / timeout notice). Unlike intermediate controls
// (EmitAsync, disposable), losing it strands the turn: the user never sees the
// reply and the progress card sits "in-flight" until a frontend restart. So
// this path trades a little latency for reliability: send the control
// (perAttemptTimeout budget) and retry on send failure (up to
// maxTerminalAttempts) with backoff. A successful POST (202) is treated as
// "delivered" — the converged single-CLI-backend shape has no ACK ingress, so
// a confirmed send on the first attempt must not regress the happy path.
//
//   - rpc performs the actual POST; nil → no-op success on attempt 1 (a
//     backend with no IPC client wired).
//   - appCtx cancels the between-attempt backoff sleep on shutdown.
//
// ctrl.PromptID is back-filled from promptID when unset. Idempotent on the
// frontend (terminal dedup keys on PromptID), so a retry is a harmless no-op
// there if the first attempt did land.
func EmitTerminalControl(logger *log.Logger, rpc backendrpc.ControlSender, appCtx context.Context, promptID, chatID string, ctrl *protocol.Control) error {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	// No IPC client wired: nothing to send, so a single no-op success is the
	// correct "delivered" signal.
	if rpc == nil {
		return nil
	}
	var lastErr error
	for attempt := 1; attempt <= maxTerminalAttempts; attempt++ {
		if attempt > 1 {
			eventmetrics.TerminalEmitRetries.Inc()
			if logger != nil {
				logger.Warn("terminal control unconfirmed, retrying",
					log.FieldChatID, chatID, log.FieldPromptID, promptID,
					log.FieldControlType, ctrl.Type, "attempt", attempt)
			}
			select {
			case <-time.After(terminalRetryBackoff(attempt)):
			case <-appCtx.Done():
				return ctxOrLast(appCtx.Err(), lastErr)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), perAttemptTimeout)
		sendErr := rpc.SendControl(ctx, ctrl)
		cancel()
		if sendErr != nil {
			lastErr = sendErr
			if logger != nil {
				logger.Warn("terminal control send failed",
					log.FieldChatID, chatID, log.FieldPromptID, promptID,
					log.FieldControlType, ctrl.Type, log.FieldError, sendErr, "attempt", attempt)
			}
			continue
		}
		// Send accepted (202). No ACK ingress in the converged backend shape,
		// so a successful POST is "delivered".
		return nil
	}
	return lastErr
}

// ctxOrLast returns ctxErr when non-nil (the shutdown cause), else lastErr so
// the caller sees the actionable send failure rather than a bare context.
func ctxOrLast(ctxErr, lastErr error) error {
	if ctxErr != nil {
		return ctxErr
	}
	return lastErr
}

// terminalRetryBackoff returns the sleep before the Nth retry (2<=attempt<=
// maxTerminalAttempts): 1s, 2s, 4s, ... capped at 4s. Attempt 1 does not sleep
// (the caller skips the backoff on the first try).
func terminalRetryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 2:
		return 1 * time.Second
	case attempt == 3:
		return 2 * time.Second
	default:
		return 4 * time.Second
	}
}

const (
	// maxTerminalAttempts bounds retries on the terminal control. 4 attempts
	// (1 initial + 3 retries) at the backoff ladder total ~7s of sleeps + up
	// to 4×5s send budgets ≈ <30s worst case — well under the user's perceived
	// "stuck" threshold while tolerating a frontend blip.
	maxTerminalAttempts = 4
	// perAttemptTimeout bounds each SendControl HTTP POST.
	perAttemptTimeout = 5 * time.Second
)
