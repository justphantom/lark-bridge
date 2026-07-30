package eventmetrics

import "sync"

// unknownStore is a concurrent map of string → *Counter, used for
// per-(backend,event_type) UnknownEvent counters.
type unknownStore struct {
	mu    sync.RWMutex
	items map[string]*Counter
}

func newUnknownStore() *unknownStore {
	return &unknownStore{items: make(map[string]*Counter)}
}

func (s *unknownStore) get(key string) *Counter {
	s.mu.RLock()
	c, ok := s.items[key]
	s.mu.RUnlock()
	if ok {
		return c
	}
	s.mu.Lock()
	// Double-check after acquiring write lock.
	if c, ok := s.items[key]; ok {
		s.mu.Unlock()
		return c
	}
	c = &Counter{}
	s.items[key] = c
	s.mu.Unlock()
	return c
}

func (s *unknownStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.items)
}
