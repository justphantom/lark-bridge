package bridgebase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/justphantom/lark-bridge/internal/backendrpc"
	"github.com/justphantom/lark-bridge/internal/eventmetrics"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// EmitTerminalControl sends one terminal Control with a retry+ACK budget.
//
// The terminal control is the single most important message of a turn (the
// final reply / error / timeout notice). Unlike intermediate controls
// (EmitAsync, disposable), losing it strands the turn: the user never sees the
// reply and the progress card sits "in-flight" until a frontend restart. So
// this path trades a little latency for reliability:
//
//  1. Send the control (perAttemptTimeout budget).
//
//  2. Wait up to ackWaitBudget for the frontend's ACK (protocol.TypeAck,
//     carried on the SSE stream). The ACK resolves acks and the loop exits
//     early — success.
//
//  3. On timeout/no-ACK, retry (up to maxTerminalAttempts) with backoff.
//
//  4. If all attempts go unconfirmed, return the last error (or a synthetic
//     "no ACK" error); the caller emits a fallback notice and bumps
//     TerminalEmitLost.
//
//     - rpc performs the actual POST; nil → no-op success on attempt 1 (a
//     backend with no IPC client wired, where nothing will ever ACK either).
//     - acks pairs the emit with the frontend's delivery ACK; nil skips the ACK
//     wait (pure retry-on-send-error) — used by miniagent, which has no
//     AckRegistry. A successful POST is "delivered" on the first attempt
//     because a confirmation that can never arrive must not regress the happy
//     path.
//     - appCtx cancels the between-attempt backoff sleep on shutdown.
//
// ctrl.PromptID is back-filled from promptID when unset. Idempotent on the
// frontend (terminal dedup keys on PromptID), so a retry is a harmless no-op
// there if the first attempt did land.
func EmitTerminalControl(logger *log.Logger, rpc backendrpc.ControlSender, acks *AckRegistry, appCtx context.Context, promptID, chatID string, ctrl *protocol.Control) error {
	if ctrl.PromptID == "" {
		ctrl.PromptID = promptID
	}
	// No IPC client wired: nothing to send and nothing to ever ACK, so a single
	// no-op success is the correct "delivered" signal.
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
		// Send accepted (202). No ACKer → pure-retry-on-send-error: a
		// successful POST with no ACK ingress is "delivered" (miniagent has no
		// AckRegistry; a nil rpc was handled above).
		if acks == nil {
			return nil
		}
		// Wait for the frontend's ACK. Resolve = success; timeout = retry.
		// ErrAckRegistryClosed = shutdown: stop retrying immediately — the
		// control was NOT confirmed delivered, so report it as undelivered
		// (pre-Close-drain this path returned success and inflated the ACK
		// metric while skipping the remaining attempts).
		err := acks.WaitFor(promptID, ackWaitBudget)
		acks.Forget(promptID)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrAckRegistryClosed) {
			return err
		}
		lastErr = fmt.Errorf("terminal control %s: no ACK within %s", ctrl.Type, ackWaitBudget)
	}
	return lastErr
}

// ctxOrLast returns ctxErr when non-nil (the shutdown cause), else lastErr so
// the caller sees the actionable send/ACK failure rather than a bare context.
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
	// to 4×5s send budgets + 4×6s ACK waits ≈ <60s worst case — well under the
	// user's perceived "stuck" threshold while tolerating a frontend blip.
	maxTerminalAttempts = 4
	// perAttemptTimeout bounds each SendControl HTTP POST.
	perAttemptTimeout = 5 * time.Second
	// ackWaitBudget bounds the wait for the frontend's ACK after a 202 accept.
	// Generous vs. a card render (<1s typical) but short enough that a
	// no-ACK frontend fails fast into the retry path instead of hanging.
	ackWaitBudget = 6 * time.Second
)
