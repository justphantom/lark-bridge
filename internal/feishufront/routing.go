package feishufront

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/atomicwrite"
)

// routeEntry is one chatID → backendID mapping with the moment it was set.
// lastAccess is the in-memory activity signal (refreshed on Resolve/Set);
// it is NOT persisted, so a restart reseeds it from UpdatedAt (see Load).
type routeEntry struct {
	BackendID  string    `json:"backendID"`
	UpdatedAt  int64     `json:"updatedAt"`
	lastAccess time.Time `json:"-"`
}

// routingFile is the on-disk shape of the Layer-1 routing table.
type routingFile struct {
	Routes map[string]routeEntry `json:"routes"`
}

// Layer1Router maps a Feishu chatID to its bound backendID. Each chat must
// be explicitly bound via Set (typically through the /backend use command);
// there is no auto-bind. The table is persisted atomically on every mutation
// and loaded on startup so routing survives process restarts.
type Layer1Router struct {
	mu     sync.RWMutex
	path   string
	routes map[string]routeEntry
	// saveMu serializes every atomicwrite.Write(r.path, …). atomicwrite's
	// temp file name is path+".tmp" (fixed), so concurrent writers to the
	// same path must not overlap — saveMu enforces that contract here.
	saveMu sync.Mutex
}

// NewLayer1Router builds a router. path may be "" for an in-memory router
// (no persistence).
func NewLayer1Router(path string) (*Layer1Router, error) {
	r := &Layer1Router{
		path:   path,
		routes: make(map[string]routeEntry),
	}
	if path != "" {
		if err := r.Load(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Load reads the persisted routing table. A missing file initialises an
// empty table (not an error).
func (r *Layer1Router) Load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path == "" {
		return nil
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("layer1: load %s: %w", r.path, err)
	}
	var f routingFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("layer1: parse %s: %w", r.path, err)
	}
	if f.Routes != nil {
		// lastAccess is not persisted: reseed it from UpdatedAt so the first
		// Prune tick after a restart does not treat every freshly-loaded
		// entry as stale (lastAccess zero-value == unix epoch).
		for k, e := range f.Routes {
			e.lastAccess = time.Unix(e.UpdatedAt, 0)
			f.Routes[k] = e
		}
		r.routes = f.Routes
	}
	return nil
}

// Save writes the routing table atomically.
func (r *Layer1Router) Save() error {
	r.mu.RLock()
	f := routingFile{Routes: make(map[string]routeEntry, len(r.routes))}
	for k, v := range r.routes {
		f.Routes[k] = v
	}
	r.mu.RUnlock()
	return r.persistFile(f)
}

// Resolve returns the backendID for chatID. Returns an error when the chat
// has no explicit binding — the caller should prompt the user to use
// /backend use {id}.
//
// Resolve also refreshes the entry's lastAccess so Prune treats a chat that
// is still receiving messages as active. This needs the write lock (not the
// read lock NewLayer1Router's other paths use): routeEntry is a value stored
// in the map, so the refreshed copy must be written back. The frontend is a
// single process driven by human-paced message volume, so write-lock
// contention is negligible (BackendRegistry/dedupSet are likewise Mutex-only).
func (r *Layer1Router) Resolve(chatID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.routes[chatID]
	if !ok {
		return "", fmt.Errorf("layer1: chat %s has no backend binding (use /backend use {id})", chatID)
	}
	e.lastAccess = time.Now()
	r.routes[chatID] = e
	return e.BackendID, nil
}

// Set binds chatID to backendID and persists.
func (r *Layer1Router) Set(chatID, backendID string) error {
	now := time.Now()
	r.mu.Lock()
	r.routes[chatID] = routeEntry{BackendID: backendID, UpdatedAt: now.Unix(), lastAccess: now}
	r.mu.Unlock()
	return r.Save()
}

// persistFile serializes f and writes it under saveMu so concurrent
// Resolve/Set saves never overlap on the shared temp file.
func (r *Layer1Router) persistFile(f routingFile) error {
	if r.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	r.saveMu.Lock()
	defer r.saveMu.Unlock()
	return atomicwrite.Write(r.path, data, 0o600)
}

// ChatsOf returns every chatID bound to backendID.
func (r *Layer1Router) ChatsOf(backendID string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for chatID, e := range r.routes {
		if e.BackendID == backendID {
			out = append(out, chatID)
		}
	}
	return out
}

// layer1PruneInterval is how often StartPrune sweeps stale routes. The table
// is small (one entry per bound chat), so a 10-minute scan is negligible;
// mirrors usage.pruneInterval's cadence.
const layer1PruneInterval = 10 * time.Minute

// Prune removes every entry whose lastAccess is older than ttl — i.e. chats
// that have neither sent a message (Resolve) nor switched backend (Set)
// within the window. Returns the count removed. When it removes anything it
// Save()s once so the pruned state lands on disk; the hot path (Resolve)
// never triggers a Save. Exported for tests and any caller needing an
// immediate sweep.
func (r *Layer1Router) Prune(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	cutoff := time.Now().Add(-ttl)
	r.mu.Lock()
	n := 0
	for id, e := range r.routes {
		if e.lastAccess.Before(cutoff) {
			delete(r.routes, id)
			n++
		}
	}
	r.mu.Unlock()
	if n > 0 {
		_ = r.Save()
	}
	return n
}

// StartPrune launches a goroutine that calls Prune(ttl) every
// layer1PruneInterval until ctx is cancelled. Panic-safe (a stray panic is
// recovered and terminates only the sweeper, not the frontend process) so a
// Prune bug cannot crash the bot. Call once at startup.
func (r *Layer1Router) StartPrune(ctx context.Context, ttl time.Duration) {
	go func() {
		defer func() { _ = recover() }()
		t := time.NewTicker(layer1PruneInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				r.Prune(ttl)
			}
		}
	}()
}
