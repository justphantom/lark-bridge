package opencodeservebridge

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// rescueTimeout bounds the HTTP round-trip for ListMessages when the OnIdle
// watchdog fires. The serve /message endpoint normally returns in tens of
// ms; 5s leaves room for a slow link without wedging the SDK watchdog tick
// (probe interval 30s — staying well under it keeps the watchdog healthy).
const rescueTimeout = 5 * time.Second

// rescueListLimit caps the message history scan for the final assistant
// reply. opencode emits one assistant message per agent step; a multi-step
// tool-using turn accumulates several, but the terminal step's FinalText is
// the final reply. 20 covers a long turn while bounding payload size.
const rescueListLimit = 20

// turnContext is the per-turn metadata the OnIdle rescue path needs to
// re-emit a TypeResult without going through streamRun. The sessionID is the
// map key; it is duplicated here so handleRescue can stamp it on the result
// card without a second lookup. rescued gates emitTerminal's default branch
// so the rescue path and the normal pump drain never both fire.
//
// Passed by pointer across the rescue seam: atomic.Bool makes the struct
// non-copyable (vet-enforced), so RegisterTurn / rescueSink / handleRescue
// all take *turnContext.
type turnContext struct {
	chatID    string
	replyToID string
	modelSpec string
	sessionID string
	rescued   atomic.Bool
}

// turnState bundles the per-turn rescue registry fields so Agent stays
// focused on SDK plumbing. All fields are guarded by their own mutex;
// embedded in Agent, with the rescue methods defined below on *Agent
// via field promotion. Tests that read a.rescue / a.turns directly
// (e.g. asserting the auto-wire happened) also reach them through
// promotion.
type turnState struct {
	// turns maps a live sessionID → its owning turn's metadata, so the
	// OnIdle rescue callback (fired with only a sessionID) can re-route a
	// recovered reply to the right chatID. Cleared when the turn ends.
	turnsMu sync.Mutex
	turns   map[string]*turnContext

	// rescue is the emit path Handler injects; nil in tests that don't
	// wire rescue (handleOnIdle treats nil as "drop silently").
	rescueMu sync.Mutex
	rescue   rescueSink
}

// turnRegistry is the optional opencodeAPI capability that maps a live
// sessionID back to its owning turn. Production *Agent implements it; test
// fakes need not — Handler treats a missing implementation as "no rescue
// path" and streamRun's rescue calls become no-ops.
type turnRegistry interface {
	RegisterTurn(sessionID string, turn *turnContext)
	UnregisterTurn(sessionID string)
	LookupTurn(sessionID string) *turnContext
}

// rescueSink is the Handler → Agent emit seam: when OnIdle rescue fires,
// the Agent hands the recovered reply back to the Handler, which assembles
// the TypeResult and emits it via the backendrpc client. modelSpec passes
// through so the result card matches the pinned model.
type rescueSink func(ctx context.Context, turn *turnContext, reply, modelSpec string)

// rescuable is the optional opencodeAPI capability for wiring a rescueSink.
// *Agent implements it; fakes need not.
type rescuable interface {
	SetRescueSink(fn rescueSink)
}

// RegisterTurn records turn metadata under sessionID. Idempotent: a re-
// registration overwrites the previous entry (defensive against the
// unlikely case streamRun sees two events carrying the same sessionID).
// The rescued flag is preserved across overwrite — a turn already rescued
// stays rescued.
func (a *Agent) RegisterTurn(sessionID string, turn *turnContext) {
	if sessionID == "" || turn == nil {
		return
	}
	a.turnsMu.Lock()
	prev := a.turns[sessionID]
	entry := &turnContext{
		chatID:    turn.chatID,
		replyToID: turn.replyToID,
		modelSpec: turn.modelSpec,
		sessionID: sessionID,
	}
	if prev != nil {
		entry.rescued.Store(prev.rescued.Load())
	}
	a.turns[sessionID] = entry
	a.turnsMu.Unlock()
}

// UnregisterTurn drops the entry for sessionID. Idempotent.
func (a *Agent) UnregisterTurn(sessionID string) {
	if sessionID == "" {
		return
	}
	a.turnsMu.Lock()
	delete(a.turns, sessionID)
	a.turnsMu.Unlock()
}

// LookupTurn returns the live turnContext pointer for sessionID, or nil if
// no turn is registered. Callers may read fields directly; mutation of the
// returned pointer is governed by the rescue flow (only handleOnIdle flips
// rescued, only streamRun's caller removes the entry).
func (a *Agent) LookupTurn(sessionID string) *turnContext {
	a.turnsMu.Lock()
	defer a.turnsMu.Unlock()
	return a.turns[sessionID]
}

// SetRescueSink wires the Handler's emit path so handleOnIdle can deliver a
// TypeResult without depending on Handler internals. Called once at Handler
// construction (NewWithLogger auto-injects when api implements rescuable).
// Read under rescueMu so a later swap remains race-free.
func (a *Agent) SetRescueSink(fn rescueSink) {
	a.rescueMu.Lock()
	a.rescue = fn
	a.rescueMu.Unlock()
}

// handleOnIdle is the SDK GlobalEventStream.OnIdle callback. It looks up the
// turn for sessionID, asks the serve server for the latest assistant reply,
// and — when one is present — emits TypeResult via the rescueSink. A no-op
// when no turn is registered (no chatID to route to), the turn is already
// rescued (pump drained first), or no assistant text is available yet (let
// the next watchdog tick retry).
//
// Runs on the SDK watchdog goroutine; returns within rescueTimeout so the
// watchdog stays schedulable. panics are absorbed by the SDK's callOnIdle
// recover.
func (a *Agent) handleOnIdle(sessionID string, _ time.Time) {
	if sessionID == "" {
		return
	}
	a.turnsMu.Lock()
	turn := a.turns[sessionID]
	a.turnsMu.Unlock()
	if turn == nil || turn.rescued.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rescueTimeout)
	defer cancel()
	msgs, err := a.client.ListMessages(ctx, sessionID, &oc.ListMessagesOpt{Limit: rescueListLimit})
	if err != nil {
		a.logger.Warn("OnIdle rescue: ListMessages failed", "sid", sessionID, "error", err)
		return
	}
	reply := pickLastAssistantText(msgs)
	if reply == "" {
		return
	}
	// Race guard against the streamRun path returning while ListMessages
	// was in flight (SSE reconnect draining a terminal event, or a user
	// abort): once UnregisterTurn has dropped the entry, the local turn
	// pointer is stale and emitTerminal's default branch has already
	// produced (or is about to produce) the TypeResult. Re-checking the
	// map identity — not just existence — defends against a re-registered
	// entry under the same sessionID landing as a fresh pointer.
	//
	// The CAS on rescued hardens against two concurrent watchdog ticks on
	// the same sessionID (rare under SDK reconnect storms): only the
	// first CAS false→true proceeds, the second sees true and bails.
	// CAS, not Store, so the post-condition "exactly one sink call per
	// turn" holds even when the re-check passes for both ticks (their
	// turns pointers are equal, so both would otherwise reach here).
	a.turnsMu.Lock()
	stillRegistered := a.turns[sessionID] == turn
	a.turnsMu.Unlock()
	if !stillRegistered || !turn.rescued.CompareAndSwap(false, true) {
		return
	}
	a.rescueMu.Lock()
	sink := a.rescue
	a.rescueMu.Unlock()
	if sink == nil {
		return
	}
	sink(ctx, turn, reply, turn.modelSpec)
}

// pickLastAssistantText scans the message list and returns the FinalText of
// the last assistant message with non-empty text. opencode emits one
// assistant message per agent step (append-only, served in creation order),
// so overwriting on each non-empty assistant FinalText yields the latest
// step's reply. Mirrors the SDK's own integration_test.go pattern.
func pickLastAssistantText(msgs []oc.SessionMessage) string {
	var reply string
	for _, m := range msgs {
		if m.Info.Role != "assistant" {
			continue
		}
		if txt := m.FinalText(); txt != "" {
			reply = txt
		}
	}
	return reply
}
