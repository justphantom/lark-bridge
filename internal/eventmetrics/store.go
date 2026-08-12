package eventmetrics

import "sync"

// maxUnknownKeys bounds how many distinct keys a store tracks before folding
// further unseen keys into a single overflow counter. The LineTruncated store
// keys on backend only (a handful of keys), so the cap is a backstop against
// unbounded growth rather than a routinely-engaged limit.
const maxUnknownKeys = 256

// unknownStore is a concurrent map of string → *Counter, used for per-backend
// counters such as LineTruncated.
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
	// tracked keys keep their own counters; only newly-seen keys overflow.
	if len(s.items) >= maxUnknownKeys {
		return s.overflow
	}
	c = &Counter{}
	s.items[key] = c
	return c
}

func (s *unknownStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.items)
	s.overflow.Reset()
}
