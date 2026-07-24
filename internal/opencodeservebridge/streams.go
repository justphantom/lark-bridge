package opencodeservebridge

import (
	"path/filepath"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// streamFor returns the event stream for loc's directory, creating it on
// first use. The v1 event bus is isolated by directory: a stream only
// receives events for sessions under its own directory, so each working
// directory needs a dedicated stream. nil loc (empty directory) maps to the
// server-default-directory stream.
//
// The pool is bounded by maxConcurrentStreams: when full, the
// least-recently-used stream is evicted and closed before the new one is
// created, so long-running processes cannot accumulate one connection per
// directory ever /cd-ed. lastUsed is updated on every hit (including the
// creation hit) so the eviction order reflects actual use, not creation
// order.
func (a *Agent) streamFor(loc *oc.LocationRef) *oc.GlobalEventStream {
	key := ""
	if loc != nil {
		key = filepath.Clean(loc.Directory)
	}
	a.streamsMu.Lock()
	defer a.streamsMu.Unlock()
	if e, ok := a.streams[key]; ok {
		e.lastUsed = time.Now()
		return e.s
	}
	// Pool at capacity: evict the least-recently-used entry. The map is
	// small (maxConcurrentStreams=8) so a linear scan is cheaper than a
	// heap and avoids the bookkeeping of an intrusive LRU list.
	if len(a.streams) >= maxConcurrentStreams {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, e := range a.streams {
			if first || e.lastUsed.Before(oldest) {
				oldestKey, oldest, first = k, e.lastUsed, false
			}
		}
		if old, ok := a.streams[oldestKey]; ok {
			delete(a.streams, oldestKey)
			// Close outside the lock would reduce contention, but Close is
			// fast (one SDK call) and doing it under the lock keeps the
			// pool's observed size consistent for a concurrent streamFor.
			_ = old.s.Close()
		}
	}
	s, _ := a.client.NewGlobalEventStream(a.appCtx, loc)
	a.streams[key] = &streamEntry{s: s, lastUsed: time.Now()}
	return s
}
