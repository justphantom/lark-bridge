package feishufront

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/atomicwrite"
)

// routeEntry is one chatID → backendID mapping with the moment it was set.
type routeEntry struct {
	BackendID string `json:"backendID"`
	UpdatedAt int64  `json:"updatedAt"`
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
func (r *Layer1Router) Resolve(chatID string) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.routes[chatID]; ok {
		return e.BackendID, nil
	}
	return "", fmt.Errorf("layer1: chat %s has no backend binding (use /backend use {id})", chatID)
}

// Set binds chatID to backendID and persists.
func (r *Layer1Router) Set(chatID, backendID string) error {
	r.mu.Lock()
	r.routes[chatID] = routeEntry{BackendID: backendID, UpdatedAt: time.Now().Unix()}
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
