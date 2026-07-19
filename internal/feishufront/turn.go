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
	BackendID string
	Model     string
	SessionID string
	StartedAt time.Time // progress/result footer elapsed time source
}

// interactiveEntry pairs a card's messageID with its bind time so the TTL
// sweeper can evict ignored cards. promptID links the card back to the turn
// whose backend interaction triggered it, so the result card can finalise it.
// reusesProgress marks a card that took over its turn's progress card in place
// (same messageID): its terminal state is the result card itself, so
// finalizeLinkedInteractive must not overwrite it with a standalone "已完成" card.
type interactiveEntry struct {
	messageID      string
	boundAt        time.Time
	promptID       string
	reusesProgress bool
}

// InteractiveBinding is the finalized-callers' view of one pending interactive
// card linked to a prompt.
type InteractiveBinding struct {
	RequestID      string
	MessageID      string
	ReusesProgress bool
}

// TurnManager tracks promptID → Turn (progress card) plus requestID →
// interactive-card binding. All access is goroutine-safe.
type TurnManager struct {
	mu           sync.RWMutex
	turns        map[string]*Turn
	interactive  map[string]interactiveEntry // requestID → interactive card binding
	typeResolver func(backendID string) string
}

// NewTurnManager builds an empty manager.
func NewTurnManager() *TurnManager {
	return &TurnManager{
		turns:       make(map[string]*Turn),
		interactive: make(map[string]interactiveEntry),
	}
}

// SetTypeResolver wires a backendID→backendType lookup (typically
// *BackendRegistry.BackendType). When set, InFlight excludes turns whose
// backendType is "deploy-monitor": a /deploy turns into `make deploy`, which
// itself calls /v1/status — counting the monitor's own turn would deadlock
// the deploy (deploy.sh refuses while inflight>0). Safe to call once at startup.
func (m *TurnManager) SetTypeResolver(fn func(backendID string) string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.typeResolver = fn
}

// Start records the progress card for one prompt.
func (m *TurnManager) Start(promptID, chatID, messageID, backendID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns[promptID] = &Turn{
		PromptID:  promptID,
		ChatID:    chatID,
		MessageID: messageID,
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

// TurnsByBackend returns the promptIDs of in-flight turns owned by backendID.
// Retained for abort/diagnostic paths that need to target one backend's turns
// — OnBackendOffline no longer releases them (a turn ends only on
// /session-abort, not on its backend disconnecting).
func (m *TurnManager) TurnsByBackend(backendID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var ids []string
	for promptID, t := range m.turns {
		if t.BackendID == backendID {
			ids = append(ids, promptID)
		}
	}
	return ids
}

// InFlight returns the number of currently in-flight turns (prompts that have
// started but not yet reached their terminal control). Used by the deploy-time
// status endpoint to let an operator avoid restarting the frontend while a
// conversation is mid-flight.
//
// Turns owned by a "deploy-monitor" backend are excluded: a /deploy prompt
// triggers `make deploy`, which queries this endpoint — counting the monitor's
// own turn would block every deploy (deploy.sh refuses while inflight>0).
func (m *TurnManager) InFlight() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	resolve := m.typeResolver
	if resolve == nil {
		return len(m.turns)
	}
	n := 0
	for _, t := range m.turns {
		if resolve(t.BackendID) == "deploy-monitor" {
			continue
		}
		n++
	}
	return n
}

// InFlightTurns returns a snapshot of every currently in-flight turn. Unlike
// InFlight it does NOT exclude deploy-monitor backends: the per-turn detail
// (promptID/chatID/backendID) is what lets an operator see a turn stranded by
// a crashed backend — the count alone hides it. Returns value-copies so the
// caller may read fields without a lock.
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
// grey forever. reusesProgress marks a card that replaced its turn's progress
// card in place (same messageID); such a card is finalised by the result card
// itself, not by a separate "已完成" flip. Callers pair this with
// SweepInteractive to evict expired bindings (and any paired card state) so
// the set cannot grow without bound.
func (m *TurnManager) BindInteractive(requestID, messageID, promptID string, reusesProgress bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interactive[requestID] = interactiveEntry{messageID: messageID, boundAt: time.Now(), promptID: promptID, reusesProgress: reusesProgress}
}

// InteractiveMessageID returns the interactive card messageID for requestID.
func (m *TurnManager) InteractiveMessageID(requestID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.interactive[requestID]
	return e.messageID, ok
}

// InteractiveByPromptID returns every still-pending interactive card linked to
// promptID. Used by sendResult to finalise those cards once the turn they
// belong to completes.
func (m *TurnManager) InteractiveByPromptID(promptID string) []InteractiveBinding {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []InteractiveBinding
	for rid, e := range m.interactive {
		if e.promptID == promptID {
			out = append(out, InteractiveBinding{RequestID: rid, MessageID: e.messageID, ReusesProgress: e.reusesProgress})
		}
	}
	return out
}

// UnbindInteractive removes the interactive card binding for requestID. Called
// once the card has been submitted so the entry does not leak.
func (m *TurnManager) UnbindInteractive(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.interactive, requestID)
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
