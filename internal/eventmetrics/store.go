package eventmetrics

import "sync"

// maxUnknownKeys bounds how many distinct keys a store tracks before folding
// further unseen keys into a single overflow counter. UnknownEvent's
// eventType comes straight from CLI-subprocess stdout; a buggy or hostile
// backend emitting random type strings would otherwise grow items without
// bound and wreck the metrics cardinality assumption. 256 is far above any
// legitimate backend's event vocabulary. The LineTruncated store keys on
// backend only (≤4 keys), so the cap never engages for it.
const maxUnknownKeys = 256

// unknownStore is a concurrent map of string → *Counter, used for
// per-(backend,event_type) UnknownEvent counters.
type unknownStore struct {
	mu       sync.RWMutex
	items    map[string]*Counter
	overflow *Counter
}

func newUnknownStore() *unknownStore {
	return &unknownStore{
		items:    make(map[string]*Counter),
		overflow: &Counter{},
	}
}

func (s *unknownStore) get(key string) *Counter {
	s.mu.RLock()
	c, ok := s.items[key]
	s.mu.RUnlock()
	if ok {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Double-check after acquiring write lock.
	if c, ok := s.items[key]; ok {
		return c
	}
	// Cap reached: fold this and all further unseen keys into one shared
	// overflow counter rather than growing the map without bound. Existing
	// tracked keys keep their own counters; only newly-seen types overflow.
	if len(s.items) >= maxUnknownKeys {
		return s.overflow
	}
	c = &Counter{}
	s.items[key] = c
	return c
}

// Overflow returns the shared counter that keys past the cap fold into. Nil
// before the store is constructed (it never is in practice); exposed so a
// future metrics exporter can surface "how many unknown events were
// collapsed" distinctly from the per-type breakdown.
func (s *unknownStore) Overflow() *Counter { return s.overflow }

func (s *unknownStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.items)
	s.overflow.Reset()
}
