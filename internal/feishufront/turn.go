package feishufront

import (
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
)

// Turn tracks one in-flight prompt and the progress card it owns.
type Turn struct {
	PromptID  string
	ChatID    string
	MessageID string // progress card message_id
	CardID    string // CardKit entity id behind the progress card (cardkit engine); "" under legacy
	BackendID string
	Model     string
	SessionID string
	StartedAt time.Time // progress/result footer elapsed time source
}

// interactiveEntry pairs a card's messageID with its bind time so the TTL
// sweeper can evict ignored cards. promptID links the card back to the turn
// whose backend interaction triggered it, so the result card can finalise it.
type interactiveEntry struct {
	messageID string
	cardID    string // CardKit entity id (cardkit engine); "" under legacy
	boundAt   time.Time
	promptID  string
}

// InteractiveBinding is the finalized-callers' view of one pending interactive
// card linked to a prompt.
type InteractiveBinding struct {
	RequestID string
	MessageID string
	CardID    string // CardKit entity id (cardkit engine); "" under legacy
}

// TurnManager tracks promptID → Turn (progress card) plus requestID →
// interactive-card binding. All access is goroutine-safe.
type TurnManager struct {
	mu          sync.RWMutex
	turns       map[string]*Turn
	interactive map[string]interactiveEntry // requestID → interactive card binding
}

// NewTurnManager builds an empty manager.
func NewTurnManager() *TurnManager {
	return &TurnManager{
		turns:       make(map[string]*Turn),
		interactive: make(map[string]interactiveEntry),
	}
}

// Start records the progress card for one prompt. cardID is the CardKit
// entity the progress card was sent as ("" under the legacy engine); the
// update path hands it back to UpdateCard.
func (m *TurnManager) Start(promptID, chatID, messageID, cardID, backendID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns[promptID] = &Turn{
		PromptID:  promptID,
		ChatID:    chatID,
		MessageID: messageID,
		CardID:    cardID,
		BackendID: backendID,
		StartedAt: time.Now(),
	}
}

// Get returns a snapshot copy of the Turn for promptID. Returning by value
// (not a pointer) lets callers read fields without a lock: SetSession may
// mutate the stored *Turn under the write lock concurrently, and a pointer
// would race against such reads. The snapshot is immutable.
func (m *TurnManager) Get(promptID string) (Turn, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.turns[promptID]
	if !ok {
		return Turn{}, false
	}
	return *t, true
}

// SetSession updates the Turn's SessionID/Model under the write lock.
func (m *TurnManager) SetSession(promptID, sessionID, model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.turns[promptID]; ok {
		t.SessionID = sessionID
		if model != "" {
			t.Model = model
		}
	}
}

// Finish removes the prompt's Turn.
func (m *TurnManager) Finish(promptID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.turns, promptID)
}

// ReclaimBackend finishes every in-flight turn owned by backendID and returns
// the reclaimed turns. Called once a backend has been confirmed offline for the
// whole offline-notice debounce window (fireOfflineNotice): a backend that
// stays down that long has lost any in-flight goroutine, so its turns can never
// receive a terminal control and would otherwise strand forever, wedging
// /v1/deploy-preflight. A brief offline blip does NOT reach here — the debounce
// timer is cancelled by OnBackendOnline, so flapping backends reclaim nothing.
// Returns value-copies so the caller can render a per-turn failure card.
func (m *TurnManager) ReclaimBackend(backendID string) []Turn {
	m.mu.Lock()
	defer m.mu.Unlock()
	var reclaimed []Turn
	for promptID, t := range m.turns {
		if t.BackendID == backendID {
			reclaimed = append(reclaimed, *t)
			delete(m.turns, promptID)
		}
	}
	return reclaimed
}

// InFlight returns the number of currently in-flight turns (prompts that have
// started but not yet reached their terminal control). Used by the deploy-time
// status endpoint to let an operator avoid restarting the frontend while a
// conversation is mid-flight.
func (m *TurnManager) InFlight() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.turns)
}

// InFlightTurns returns a snapshot of every currently in-flight turn. The
// per-turn detail (promptID/chatID/backendID) is what lets an operator see a
// turn stranded by a crashed backend — the count alone hides it. Returns
// value-copies so the caller may read fields without a lock.
func (m *TurnManager) InFlightTurns() []Turn {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Turn, 0, len(m.turns))
	for _, t := range m.turns {
		out = append(out, *t)
	}
	return out
}

// BindInteractive records the messageID of an interactive card by requestID.
// promptID links it to the turn whose backend interaction triggered the card,
// so the result card can flip it to a finalised state instead of leaving it
// grey forever. Callers pair this with SweepInteractive to evict expired
// bindings (and any paired card state) so the set cannot grow without bound.
func (m *TurnManager) BindInteractive(requestID, messageID, cardID, promptID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interactive[requestID] = interactiveEntry{messageID: messageID, cardID: cardID, boundAt: time.Now(), promptID: promptID}
}

// InteractiveCardRef returns the interactive card's messageID and CardKit
// entity id for requestID.
func (m *TurnManager) InteractiveCardRef(requestID string) (string, string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.interactive[requestID]
	return e.messageID, e.cardID, ok
}

// InteractiveMessageID returns the interactive card messageID for requestID.
func (m *TurnManager) InteractiveMessageID(requestID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.interactive[requestID]
	return e.messageID, ok
}

// InteractiveCardIDByMessageID returns the CardKit entity id of the first
// interactive binding pointing at messageID, or "" under the legacy engine.
// Used by the /send multi-round refresh, which knows only the messageID.
func (m *TurnManager) InteractiveCardIDByMessageID(messageID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.interactive {
		if e.messageID == messageID {
			return e.cardID
		}
	}
	return ""
}

// UnbindInteractive removes the interactive card binding for requestID. Called
// once the card has been submitted so the entry does not leak.
func (m *TurnManager) UnbindInteractive(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.interactive, requestID)
}

// UnbindInteractiveByPromptID removes every pending interactive card binding
// linked to promptID and returns the bindings that were removed. Callers use
// the returned IDs to clean up paired card state (cached bytes, timers).
func (m *TurnManager) UnbindInteractiveByPromptID(promptID string) []InteractiveBinding {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []InteractiveBinding
	for rid, e := range m.interactive {
		if e.promptID == promptID {
			out = append(out, InteractiveBinding{RequestID: rid, MessageID: e.messageID, CardID: e.cardID})
			delete(m.interactive, rid)
		}
	}
	return out
}

// UnbindInteractiveByMessageID removes every pending interactive card binding
// pointing at messageID except keepRequestID and returns the removed requestIDs.
// Used when a multi-round picker refreshes a card in place.
func (m *TurnManager) UnbindInteractiveByMessageID(messageID, keepRequestID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for rid, e := range m.interactive {
		if e.messageID == messageID && rid != keepRequestID {
			out = append(out, rid)
			delete(m.interactive, rid)
		}
	}
	return out
}

// SweepInteractive evicts interactive bindings older than cardkit.InteractiveTimeout and
// returns the expired requestIDs so callers can drop paired state (the cached
// card bytes in Dispatcher.cards). Called on each bind; between binds the set
// is bounded by how fast new interactive cards arrive.
func (m *TurnManager) SweepInteractive() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sweepInteractiveLocked()
}

// sweepInteractiveLocked is the eviction worker; caller holds m.mu.
func (m *TurnManager) sweepInteractiveLocked() []string {
	cutoff := time.Now().Add(-cardkit.InteractiveTimeout)
	var expired []string
	for id, e := range m.interactive {
		if e.boundAt.Before(cutoff) {
			expired = append(expired, id)
			delete(m.interactive, id)
		}
	}
	return expired
}
