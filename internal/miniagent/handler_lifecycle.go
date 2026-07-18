package miniagent

import (
	"context"
	"time"
)

// closeGrace bounds how long Close waits for in-flight turns to wind down
// after cancelling them. Long enough for a final emit to land, short enough
// that a stuck goroutine does not hang SIGTERM.
const closeGrace = 5 * time.Second

// promptCancel is the cancel entry of one in-flight turn, registered under
// its chatID so busy-then-drop and Close can target exactly one chat. Local
// type (mirroring bridgebase.PromptCancel) keeps miniagent independent of the
// bridgebase package, which miniagent otherwise does not use.
type promptCancel struct {
	cancel    context.CancelFunc
	startTime time.Time
}

// RunningSession describes one in-flight turn for the /running card.
type RunningSession struct {
	ChatID   string
	Duration time.Duration
}

// RunningSessions snapshots all in-flight turns.
func (h *Handler) RunningSessions() []RunningSession {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	now := time.Now()
	out := make([]RunningSession, 0, len(h.cancelBy))
	for chatID, pc := range h.cancelBy {
		out = append(out, RunningSession{ChatID: chatID, Duration: now.Sub(pc.startTime)})
	}
	return out
}

// startTurn reserves the per-chat turn slot. Returns (turnCtx, mine, false)
// when the chat already has an in-flight turn (busy-then-drop); the caller
// must NOT touch turnCtx/mine in that case. On success turnCtx is derived
// from the process ctx so Close can cancel it, and the wg is incremented so
// Close waits for this turn.
func (h *Handler) startTurn(ctx context.Context, chatID string) (turnCtx context.Context, mine *promptCancel, ok bool) {
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
	mine = &promptCancel{cancel: cancel, startTime: time.Now()}
	h.cancelBy[chatID] = mine
	h.wg.Add(1)
	return turnCtx, mine, true
}

// endTurn releases the per-chat slot only if it still points at mine (a
// later Close or superceding turn may have already cleared it). Always
// decrements wg to match startTurn's Add.
func (h *Handler) endTurn(chatID string, mine *promptCancel) {
	h.cancelMu.Lock()
	if cur, ok := h.cancelBy[chatID]; ok && cur == mine {
		delete(h.cancelBy, chatID)
	}
	h.cancelMu.Unlock()
	h.wg.Done()
}

// Close cancels every in-flight turn and waits up to closeGrace for them to
// wind down so the process does not exit mid-emit / mid-Append. Idempotent.
func (h *Handler) Close() {
	h.closeOnce.Do(func() {
		h.cancelMu.Lock()
		h.closed = true
		for _, pc := range h.cancelBy {
			pc.cancel()
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
// and deleting here would make endTurn's `cur == mine` check fail to clean
// up. Mirrors bridgebase.Core.AbortChat's contract.
func (h *Handler) abortChat(chatID string) bool {
	h.cancelMu.Lock()
	defer h.cancelMu.Unlock()
	if pc, ok := h.cancelBy[chatID]; ok {
		pc.cancel()
		return true
	}
	return false
}
