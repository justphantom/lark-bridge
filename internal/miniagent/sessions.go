package miniagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// SessionInfo describes one stored session of a chat.
type SessionInfo struct {
	ID      string
	Bytes   int64
	ModTime time.Time
	Current bool
}

// newSessionID names a session by creation time: sortable, human-meaningful,
// and filename-safe without extra metadata.
func newSessionID(now time.Time) string {
	return now.Format("20060102-150405")
}

// resolve maps chatID to its active session file, migrating a pre-sessions
// legacy {chatID}.jsonl on first sight. Returns ("","") when the chat has
// no session at all. A failed legacy rename returns the legacy path with an
// empty sid so history stays usable in degraded mode.
func (h *History) resolve(chatID string) (sid, path string) {
	if sid := h.current(chatID); sid != "" {
		return sid, h.sessionPath(chatID, sid)
	}
	legacy := h.legacyPath(chatID)
	st, err := os.Stat(legacy)
	if err != nil {
		return "", ""
	}
	sid = "legacy-" + st.ModTime().Format("20060102-150405")
	path = h.sessionPath(chatID, sid)
	if err := os.Rename(legacy, path); err != nil {
		h.logger.Warn("miniagent history: legacy migrate rename failed", log.FieldError, err)
		return "", legacy
	}
	if err := h.writeCur(chatID, sid); err != nil {
		h.logger.Warn("miniagent history: legacy migrate pointer failed", log.FieldError, err)
	}
	return sid, path
}

// Current returns the active session id, or "" when none / memory disabled.
func (h *History) Current(chatID string) string {
	if h == nil {
		return ""
	}
	return h.current(chatID)
}

// NewSession points the chat at a fresh empty session. The old session file
// is kept and can be re-selected with UseSession. The new empty file is
// materialized immediately (not lazily) so /session-list shows it and marks
// it Current before the first message lands — otherwise a freshly-created
// session would be invisible until the next Append.
func (h *History) NewSession(chatID string) (string, error) {
	if h == nil {
		return "", errors.New("miniagent: memory disabled")
	}
	now := time.Now()
	sid := newSessionID(now)
	if sid == h.current(chatID) {
		// Two NewSession calls within one second would share a filename.
		sid = fmt.Sprintf("%s-%d", sid, now.Nanosecond())
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return "", err
	}
	// O_EXCL: a brand-new sid must never collide with an existing file; if it
	// somehow does, erroring is safer than truncating prior history.
	f, err := os.OpenFile(h.sessionPath(chatID, sid), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	if err := h.writeCur(chatID, sid); err != nil {
		return "", err
	}
	return sid, nil
}

// ListSessions enumerates the chat's session files, oldest first.
func (h *History) ListSessions(chatID string) ([]SessionInfo, error) {
	if h == nil {
		return nil, errors.New("miniagent: memory disabled")
	}
	h.resolve(chatID) // surface a legacy file as a listable session
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := sanitizeChatID(chatID) + "__"
	cur := h.current(chatID)
	var out []SessionInfo
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".jsonl")
		out = append(out, SessionInfo{ID: id, Bytes: st.Size(), ModTime: st.ModTime(), Current: id == cur})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.Before(out[j].ModTime) })
	return out, nil
}

// UseSession switches the chat back to a stored session.
func (h *History) UseSession(chatID, sid string) error {
	if h == nil {
		return errors.New("miniagent: memory disabled")
	}
	if !validSessionID(sid) {
		return fmt.Errorf("miniagent: invalid session id %q", sid)
	}
	if !h.sessionExists(chatID, sid) {
		return fmt.Errorf("miniagent: session %s not found", sid)
	}
	return h.writeCur(chatID, sid)
}

// DeleteSession removes a session file. Empty sid means the active session;
// deleting the active session also clears the pointer so the next prompt
// starts fresh.
func (h *History) DeleteSession(chatID, sid string) error {
	if h == nil {
		return errors.New("miniagent: memory disabled")
	}
	if sid == "" {
		h.resolve(chatID)
		if sid = h.current(chatID); sid == "" {
			return errors.New("miniagent: no session to delete")
		}
	}
	if !validSessionID(sid) {
		return fmt.Errorf("miniagent: invalid session id %q", sid)
	}
	if err := os.Remove(h.sessionPath(chatID, sid)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("miniagent: session %s not found", sid)
		}
		return err
	}
	if h.current(chatID) == sid {
		_ = os.Remove(h.curPathFor(chatID))
	}
	return nil
}

// validSessionID gates user-supplied ids to the charset newSessionID and
// the legacy migration produce, so a sid cannot escape the history dir.
func validSessionID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

func (h *History) sessionExists(chatID, sid string) bool {
	_, err := os.Stat(h.sessionPath(chatID, sid))
	return err == nil
}

func (h *History) current(chatID string) string {
	b, err := os.ReadFile(h.curPathFor(chatID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// writeCur stores the active session id for chatID atomically: write to a
// sibling temp file then rename, so a crash mid-write cannot leave a
// truncated/empty .cur that would make the next Load silently drop the
// session. The temp file lives in the same directory as the target so the
// rename is atomic on POSIX (same filesystem).
func (h *History) writeCur(chatID, sid string) error {
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return err
	}
	target := h.curPathFor(chatID)
	tmp, err := os.CreateTemp(h.dir, ".cur-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(sid); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Model returns the per-chat pinned model id, or "" when none is set (the
// caller falls back to the global default). Read from the .model file.
func (h *History) Model(chatID string) string {
	if h == nil {
		return ""
	}
	b, err := os.ReadFile(h.modelPath(chatID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SetModel pins model for chatID (atomic temp+rename, same pattern as
// writeCur). An empty model clears the pin (removes the .model file).
func (h *History) SetModel(chatID, model string) error {
	if h == nil {
		return errors.New("miniagent: memory disabled")
	}
	if model == "" {
		_ = os.Remove(h.modelPath(chatID))
		return nil
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return err
	}
	target := h.modelPath(chatID)
	tmp, err := os.CreateTemp(h.dir, ".model-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(model); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		cleanup()
		return err
	}
	return nil
}

func (h *History) modelPath(chatID string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+".model")
}

func (h *History) sessionPath(chatID, sid string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+"__"+sid+".jsonl")
}

func (h *History) legacyPath(chatID string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+".jsonl")
}

func (h *History) curPathFor(chatID string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+".cur")
}

// sanitizeChatID collapses any character unsafe for a filename into '_' so a
// chatID with an unexpected character cannot escape the history directory.
// Local copy of streamarchive.SanitizeName so miniagent stays independent of
// the archive package (it only needs this one helper).
func sanitizeChatID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
