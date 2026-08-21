package miniagent

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// closeGrace bounds how long Close waits for in-flight turns to wind down
// after cancelling them. Long enough for a final emit to land, short enough
// that a stuck goroutine does not hang SIGTERM.
const closeGrace = 5 * time.Second

// RunningSession describes one in-flight turn for the /running card and the
// running-session lifecycle controls sent to the frontend.
type RunningSession struct {
	PromptID  string
	ChatID    string
	StartTime time.Time
}

// RunningSessions snapshots all in-flight turns.
func (h *Handler) RunningSessions() []RunningSession {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	out := make([]RunningSession, 0, len(h.cancelBy))
	for _, pc := range h.cancelBy {
		out = append(out, RunningSession{PromptID: pc.PromptID, ChatID: pc.ChatID, StartTime: pc.StartTime})
	}
	return out
}

// startTurn reserves the per-chat turn slot. Returns (turnCtx, mine, false)
// when the chat already has an in-flight turn (busy-then-drop); the caller
// must NOT touch turnCtx/mine in that case. On success turnCtx is derived
// from the process ctx so Close can cancel it, and the wg is incremented so
// Close waits for this turn.
//
// Uses PromptCancel (the shared shape every CLI backend's
// cancel entry shares) — the local copy was byte-identical.
func (h *Handler) startTurn(ctx context.Context, chatID, promptID string) (turnCtx context.Context, mine *PromptCancel, ok bool) {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	// After Close, reject new turns so the wg.Wait in Close is not held open
	// by a late HandleEvent that slipped in between cancelAll releasing the
	// lock and the wait starting.
	if h.closed {
		return nil, nil, false
	}
	if _, busy := h.cancelBy[chatID]; busy {
		return nil, nil, false
	}
	turnCtx, cancel := context.WithCancel(ctx)
	mine = &PromptCancel{Cancel: cancel, StartTime: time.Now(), ChatID: chatID, PromptID: promptID}
	h.cancelBy[chatID] = mine
	h.wg.Add(1)
	// Announce the turn to the frontend so the running-session set stays
	// consistent with the backend's cancelBy. Fire-and-forget: a failed emit
	// does not abort the turn; the next metrics snapshot reconciles.
	h.sendTurnStarted(chatID, promptID)
	return turnCtx, mine, true
}

// endTurn releases the per-chat slot only if it still points at mine (a
// later Close or superceding turn may have already cleared it). Always
// decrements wg to match startTurn's Add.
func (h *Handler) endTurn(chatID string, mine *PromptCancel) {
	h.cancelMu.Lock()
	if cur, ok := h.cancelBy[chatID]; ok && cur == mine {
		delete(h.cancelBy, chatID)
	}
	h.cancelMu.Unlock()
	h.wg.Done()
	// Announce completion fire-and-forget. The promptID is read from mine
	// (which the caller still holds) so the frontend can remove the exact row.
	if mine != nil && mine.PromptID != "" {
		h.sendTurnFinished(mine.PromptID)
	}
}

// sendTurnStarted emits a TypeTurnStarted control. Best-effort: failures are
// logged, never propagated, so a turn cannot fail because of a control emit.
func (h *Handler) sendTurnStarted(chatID, promptID string) {
	if h.rpc == nil || promptID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeTurnStarted,
		PromptID: promptID,
		ChatID:   chatID,
		TurnStarted: &protocol.TurnStartedPayload{
			TurnInfo: protocol.TurnInfo{PromptID: promptID, ChatID: chatID},
		},
	}); err != nil {
		h.logger.Warn("turn started emit failed",
			log.FieldChatID, chatID, log.FieldPromptID, promptID, log.FieldError, err)
	}
}

// sendTurnFinished emits a TypeTurnFinished control. Best-effort.
func (h *Handler) sendTurnFinished(promptID string) {
	if h.rpc == nil || promptID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeTurnFinished,
		PromptID: promptID,
		TurnFinished: &protocol.TurnFinishedPayload{
			PromptID: promptID,
		},
	}); err != nil {
		h.logger.Warn("turn finished emit failed",
			log.FieldPromptID, promptID, log.FieldError, err)
	}
}

// Close cancels every in-flight turn and waits up to closeGrace for them to
// wind down so the process does not exit mid-emit / mid-Append. Idempotent.
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		h.appCancel()
		h.cancelMu.Lock()
		h.closed = true
		for _, pc := range h.cancelBy {
			pc.Cancel()
		}
		h.cancelMu.Unlock()
		h.answers.Drain()
		done := make(chan struct{})
		go func() {
			h.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeGrace):
		}
	})
}

// abortChat cancels the in-flight turn for chatID, if any. Returns whether a
// turn was running. It does NOT delete the cancelBy entry: the goroutine that
// owns the slot (startTurn's caller) will endTurn on its own as it unwinds,
// and deleting here would make endTurn's `cur == mine` check fail to clean up.
func (h *Handler) abortChat(chatID string) bool {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if pc, ok := h.cancelBy[chatID]; ok {
		pc.Cancel()
		return true
	}
	return false
}
