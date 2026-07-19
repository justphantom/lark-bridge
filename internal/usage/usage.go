// Package usage accumulates per-session token and cost totals and persists
// them atomically so an operator can reconstruct a session's full footprint
// across the per-turn stream archives.
//
// One Store per backend process writes its own file (usage-claude.json /
// usage-opencode.json under state_dir); the two backends share a state_dir
// but never the same file, mirroring the per-backend router split.
package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/atomicwrite"
	"github.com/justphantom/lark-bridge/internal/log"
)

// fileVersion is the on-disk format version.
const fileVersion = 1

// filePerm is the permission for the persist file (carries cost data).
const filePerm = 0o600

// Delta is the per-turn contribution one Store.Add call adds. Fields not
// relevant to a backend stay zero (e.g. opencode's Cost is usually 0).
type Delta struct {
	SessionID string
	ChatID    string
	Input     int
	Output    int
	CacheRead int
	// CacheWrite is cache_creation_input_tokens (claude) or cache.write
	// (opencode).
	CacheWrite int
	Cost       float64
	// Turns counts this Add as one turn (one CLI invocation). The caller
	// passes 1 per finalised promptResult.
	Turns int
}

// Entry is one session's accumulated totals.
type Entry struct {
	SessionID  string    `json:"sessionId"`
	ChatID     string    `json:"chatId,omitempty"`
	Input      int       `json:"input"`
	Output     int       `json:"output"`
	CacheRead  int       `json:"cacheRead"`
	CacheWrite int       `json:"cacheWrite"`
	Cost       float64   `json:"cost"`
	Turns      int       `json:"turns"`
	LastUpdate time.Time `json:"lastUpdate"`
}

// fileShape is the on-disk envelope.
type fileShape struct {
	Version  int               `json:"version"`
	Sessions map[string]*Entry `json:"sessions"`
}

// Store accumulates per-session token/cost totals and persists them to path
// atomically. Safe for concurrent use. An empty path makes it a pure in-memory
// counter (no persistence) so callers can wire it unconditionally.
type Store struct {
	path   string
	logger *log.Logger

	mu       sync.Mutex
	sessions map[string]*Entry

	// saveCh / saveStop / saveDone drive the single-worker save coalescer,
	// identical to router.Router: a non-blocking send triggers a save; if a
	// save is already pending the send is dropped because the worker always
	// reads the freshest snapshot. nil when path == "" so saveAsync is a pure
	// no-op and no goroutine is started.
	saveCh    chan struct{}
	saveStop  chan struct{}
	saveDone  chan struct{}
	closeOnce sync.Once
}

// New loads any existing totals from path and starts the save coalescer. A
// missing file initialises an empty store (not an error). logger defaults to
// a no-op when nil.
func New(path string, logger *log.Logger) (*Store, error) {
	if logger == nil {
		logger = log.Nop()
	}
	s := &Store{
		path:     path,
		logger:   logger,
		sessions: make(map[string]*Entry),
	}
	if path != "" {
		s.saveCh = make(chan struct{}, 1)
		s.saveStop = make(chan struct{})
		s.saveDone = make(chan struct{})
		if err := s.load(); err != nil {
			return nil, fmt.Errorf("usage: load %s: %w", path, err)
		}
		go s.saveLoop()
	}
	return s, nil
}

// Add accumulates d into the session's entry and schedules a save. A new
// sessionID creates the entry; an existing one adds to its totals. ChatID is
// recorded once on creation and not overwritten (a session is bound to one
// chat for its lifetime).
func (s *Store) Add(d Delta) {
	if d.SessionID == "" {
		return
	}
	now := time.Now()
	s.mu.Lock()
	e, ok := s.sessions[d.SessionID]
	if !ok {
		e = &Entry{SessionID: d.SessionID, ChatID: d.ChatID}
		s.sessions[d.SessionID] = e
	}
	e.Input += d.Input
	e.Output += d.Output
	e.CacheRead += d.CacheRead
	e.CacheWrite += d.CacheWrite
	e.Cost += d.Cost
	e.Turns += d.Turns
	e.LastUpdate = now
	s.mu.Unlock()
	s.saveAsync()
}

// Close stops the save coalescer and performs a final synchronous save so the
// last Add before shutdown is not lost. Idempotent. In-memory stores (empty
// path) are a no-op.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		if s.saveStop == nil {
			return
		}
		close(s.saveStop)
		<-s.saveDone
		if err := s.save(); err != nil {
			s.logger.Error("usage final save failed",
				log.FieldPath, s.path,
				log.FieldError, err)
		}
	})
}

// Snapshot returns a copy of every session's entry. Owned by the caller.
func (s *Store) Snapshot() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Entry, 0, len(s.sessions))
	for _, e := range s.sessions {
		out = append(out, *e)
	}
	return out
}

// Get returns a copy of the session's accumulated entry. ok is false when the
// session has no recorded history (first turn) or the store is nil.
func (s *Store) Get(sessionID string) (Entry, bool) {
	if s == nil || sessionID == "" {
		return Entry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[sessionID]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// load reads the persisted totals. A missing file is not an error. A malformed
// file or unsupported version is a hard error: returning nil would reset
// totals to zero and the next save would overwrite, permanently losing
// accumulated accounting.
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", s.path, err)
	}
	var f fileShape
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse %s: %w; back up or remove the file", s.path, err)
	}
	if f.Version != fileVersion {
		return fmt.Errorf("unsupported version %d (expected %d); back up or remove the file", f.Version, fileVersion)
	}
	if f.Sessions != nil {
		s.sessions = f.Sessions
	}
	return nil
}

// saveAsync schedules a save on the worker goroutine. Coalesces: if multiple
// Add calls happen before the worker drains the previous signal, only one
// save runs (the latest snapshot is what hits disk).
func (s *Store) saveAsync() {
	if s.saveCh == nil {
		return
	}
	select {
	case s.saveCh <- struct{}{}:
	default:
	}
}

func (s *Store) saveLoop() {
	defer close(s.saveDone)
	for {
		select {
		case <-s.saveCh:
			if err := s.save(); err != nil {
				s.logger.Error("usage state save failed in loop",
					log.FieldPath, s.path,
					log.FieldError, err)
			}
		case <-s.saveStop:
			return
		}
	}
}

// save writes the current totals atomically (tmp + fsync + rename).
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	payload := fileShape{
		Version:  fileVersion,
		Sessions: make(map[string]*Entry, len(s.sessions)),
	}
	for k, v := range s.sessions {
		cp := *v
		payload.Sessions[k] = &cp
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicwrite.Write(s.path, data, filePerm); err != nil {
		return fmt.Errorf("save %s: %w", s.path, err)
	}
	return nil
}
