package feishufront

import (
	"sync"
	"time"
)

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

func (s *dedupSet) Add(id string) bool {
	if id == "" {
		return true
	}
	now := time.Now()
	cutoff := now.Add(-s.ttl)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.seen {
		if t.Before(cutoff) {
			delete(s.seen, k)
		}
	}
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

// Configure updates the TTL and entry cap at runtime. Called by
// Dispatcher.SetDedupConfig so the dispatcher does not reach into the
// dedupSet's private fields directly.
func (s *dedupSet) Configure(ttl time.Duration, maxEntries int) {
	s.mu.Lock()
	s.ttl = ttl
	s.maxEntries = maxEntries
	s.mu.Unlock()
}
