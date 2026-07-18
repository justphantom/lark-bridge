package miniagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// Fact is one structured long-term memory entry: an explicit key/value pair
// extracted from conversation or set by the user. Unlike the raw message
// history, facts are compact, searchable, and survive across sessions.
type Fact struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FactScope controls which conversations can see a fact.
//   - chat:    only the chat that created it (default, privacy-safe).
//   - project: tied to the current workspaceRoot, shared across chats in the
//              same project.
//   - global:  shared across all chats (use sparingly; user preferences only).
type FactScope string

const (
	ScopeChat    FactScope = "chat"
	ScopeProject FactScope = "project"
	ScopeGlobal  FactScope = "global"
)

// ParseFactScope normalizes a scope string. Unknown values fall back to chat.
func ParseFactScope(s string) FactScope {
	switch FactScope(s) {
	case ScopeGlobal, ScopeProject, ScopeChat:
		return FactScope(s)
	default:
		return ScopeChat
	}
}

// FactStore persists and retrieves facts by scope. It is safe for concurrent
// use within a process. A nil *FileFactStore (memory disabled) is valid: all
// reads return empty and all writes are no-ops.
type FactStore interface {
	// Get returns one fact. ok is false when the key is absent or memory is off.
	Get(scope FactScope, chatID, key string) (Fact, bool, error)
	// Set writes or overwrites a fact. source records where it came from
	// (e.g. a session id or "memory_set tool").
	Set(scope FactScope, chatID, key, value, source string) error
	// List returns all facts matching the optional key prefix, sorted by key.
	List(scope FactScope, chatID, prefix string) ([]Fact, error)
	// Delete removes one fact. Deleting a missing key is not an error.
	Delete(scope FactScope, chatID, key string) error
	// Search does a simple substring match over keys and values. This is
	// intentionally lightweight; replace with full-text search if scale demands.
	Search(scope FactScope, chatID, query string, limit int) ([]Fact, error)
}

// FileFactStore is a JSON file-backed implementation rooted at
// {stateDir}/miniagent/memory/. Each scope/chat combination gets its own file
// so global facts do not leak into unrelated chats and project facts can be
// shared.
type FileFactStore struct {
	dir    string
	logger *log.Logger
	mu     sync.RWMutex
}

// NewFactStore builds a FileFactStore rooted at
// {stateDir}/miniagent/memory. logger may be nil.
func NewFactStore(stateDir string, logger *log.Logger) *FileFactStore {
	if logger == nil {
		logger = log.Nop()
	}
	return &FileFactStore{
		dir:    filepath.Join(stateDir, "miniagent", "memory"),
		logger: logger,
	}
}

// scopedFile returns the backing file for a scope/chat combination. The
// chatID is sanitized the same way as history filenames.
func (s *FileFactStore) scopedFile(scope FactScope, chatID string) string {
	if scope == ScopeGlobal {
		return filepath.Join(s.dir, "global.json")
	}
	if scope == ScopeProject {
		return filepath.Join(s.dir, "project.json")
	}
	return filepath.Join(s.dir, sanitizeChatID(chatID)+".json")
}

// load reads the backing file for a scope/chat. Returns an empty map when the
// file does not exist; a corrupt file is logged and treated as empty so one
// bad write does not brick the store.
func (s *FileFactStore) load(scope FactScope, chatID string) (map[string]Fact, error) {
	path := s.scopedFile(scope, chatID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Fact{}, nil
		}
		return nil, err
	}
	out := map[string]Fact{}
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		s.logger.Warn("miniagent memory: corrupt fact file, resetting", "path", path, log.FieldError, err)
		return map[string]Fact{}, nil
	}
	return out, nil
}

// save writes the backing file atomically (temp+rename) under a write lock.
func (s *FileFactStore) save(scope FactScope, chatID string, facts map[string]Fact) error {
	path := s.scopedFile(scope, chatID)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".memory-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(facts); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Get implements FactStore.
func (s *FileFactStore) Get(scope FactScope, chatID, key string) (Fact, bool, error) {
	if s == nil {
		return Fact{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return Fact{}, false, err
	}
	f, ok := facts[key]
	return f, ok, nil
}

// Set implements FactStore.
func (s *FileFactStore) Set(scope FactScope, chatID, key, value, source string) error {
	if s == nil {
		return nil
	}
	if key == "" {
		return fmt.Errorf("memory key cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return err
	}
	facts[key] = Fact{
		Key:       key,
		Value:     value,
		Source:    source,
		UpdatedAt: time.Now(),
	}
	return s.save(scope, chatID, facts)
}

// List implements FactStore.
func (s *FileFactStore) List(scope FactScope, chatID, prefix string) ([]Fact, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if prefix != "" && !strings.HasPrefix(f.Key, prefix) {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Delete implements FactStore.
func (s *FileFactStore) Delete(scope FactScope, chatID, key string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return err
	}
	if _, ok := facts[key]; !ok {
		return nil
	}
	delete(facts, key)
	// Keep the file even when empty so the store remains addressable; an empty
	// JSON object is valid and tiny.
	return s.save(scope, chatID, facts)
}

// Search implements FactStore.
func (s *FileFactStore) Search(scope FactScope, chatID, query string, limit int) ([]Fact, error) {
	if s == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	q := strings.ToLower(query)
	s.mu.RLock()
	defer s.mu.RUnlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if strings.Contains(strings.ToLower(f.Key), q) || strings.Contains(strings.ToLower(f.Value), q) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// formatFacts renders a slice of facts as a short bullet list for injection
// into the system prompt. Empty input returns an empty string.
func formatFacts(facts []Fact) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n以下是与当前对话相关的已知事实（由用户或之前的对话沉淀）：\n")
	for _, f := range facts {
		fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
	}
	return sb.String()
}
