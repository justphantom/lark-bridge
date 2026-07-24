package feishufront

import (
	"context"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
)

// dedupPruneInterval is how often StartPrune sweeps expired entries. With
// Add no longer scanning on every call, a periodic sweep is what keeps the
// set bounded in the TTL-only mode (actionIDs/terminals, maxEntries=0).
// 60s is far below the 5m TTL, so a sweep never changes which entries are
// observable as "fresh".
const dedupPruneInterval = 60 * time.Second

// dedupSet is a TTL-bounded in-memory dedup set with an optional entry cap.
type dedupSet struct {
	mu         sync.Mutex
	seen       map[string]time.Time
	ttl        time.Duration
	maxEntries int // <=0 means unbounded (TTL-only)
}

func newDedupSet(ttl time.Duration, maxEntries int) *dedupSet {
	return &dedupSet{seen: make(map[string]time.Time), ttl: ttl, maxEntries: maxEntries}
}

// Add records id and returns true when it was not present. It is O(1) on the
// steady-state path: TTL-expired entries are NOT swept here (that was O(n)
// per call). Periodic sweeping is StartPrune's job; callers that do not wire
// StartPrune rely on maxEntries to bound growth (the LRU cap evicts on
// overflow below, which is still O(n) but only reached on overflow, not on
// every call). A zero/empty id is a no-op returning true (no dedup on it).
func (s *dedupSet) Add(id string) bool {
	if id == "" {
		return true
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[id]; ok {
		return false
	}
	// Cap: if at capacity, evict the entry with the oldest timestamp. O(n)
	// but only reached on overflow, which the default cap (1000) and TTL
	// (5m) keep out of the steady-state path for normal message volume.
	if s.maxEntries > 0 && len(s.seen) >= s.maxEntries {
		var oldestKey string
		var oldestTime time.Time
		first := true
		for k, t := range s.seen {
			if first || t.Before(oldestTime) {
				oldestKey, oldestTime, first = k, t, false
			}
		}
		delete(s.seen, oldestKey)
	}
	s.seen[id] = now
	return true
}

// Delete removes id from the set so a retried delivery (after a failed first
// attempt) is reprocessed instead of dropped. Used by DispatchIncoming when an
// inbound message's handling fails before a turn is durably established: the
// SDK then ACKs 500 and Feishu redelivers, and without Delete the redelivery
// would be silently swallowed by the Add done on entry.
func (s *dedupSet) Delete(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	delete(s.seen, id)
	s.mu.Unlock()
}

// Prune removes entries whose timestamp is older than ttl. Called by
// StartPrune on its periodic tick; also exported for tests and any caller
// that wants an immediate sweep. Returns the number of entries removed.
func (s *dedupSet) Prune() int {
	cutoff := time.Now().Add(-s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, t := range s.seen {
		if t.Before(cutoff) {
			delete(s.seen, k)
			n++
		}
	}
	return n
}

// StartPrune launches a goroutine that calls Prune every
// dedupPruneInterval until ctx is cancelled. The goroutine is panic-safe
// (bridgebase.GoSafe) so a stray panic cannot crash the frontend. Call once
// per set at startup; calling twice would double the sweep rate harmlessly.
func (s *dedupSet) StartPrune(ctx context.Context) {
	bridgebase.GoSafe(nil, "dedup-prune", func() {
		ticker := time.NewTicker(dedupPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Prune()
			}
		}
	})
}

// Configure updates the TTL and entry cap at runtime. Called by
// Dispatcher.SetDedupConfig so the dispatcher does not reach into the
// dedupSet's private fields directly.
func (s *dedupSet) Configure(ttl time.Duration, maxEntries int) {
	s.mu.Lock()
	s.ttl = ttl
	s.maxEntries = maxEntries
	s.mu.Unlock()
}
