//go:build linux || darwin

package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Session is the subset of a claude session file the bridge renders. The
// Claude Code CLI persists each session as one .jsonl transcript under
// ~/.claude/projects/<encoded-cwd>/<uuid>.jsonl (plus an optional <uuid>/
// directory for blobs/sub-agents). There is no CLI-emitted title field, so
// Title is derived from the first user-side record's content (the opening
// prompt), truncated — the same shape omp's readSessionHeader produces.
type Session struct {
	ID      string
	Title   string
	Updated int64 // unix ms (file mtime)
}

// uuidRe matches a lowercase-hex UUID, the filename shape the CLI uses for a
// session transcript. It filters out stray non-session files (a future CLI
// layout change, an editor swap file, ...) so a malformed name never surfaces
// as a bogus session id.
var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// maxSessionTitleRunes caps the derived title so one verbose opening prompt
// cannot blow out the /session-list row width. Mirrors opencode's display cap.
const maxSessionTitleRunes = 60

// encodeProjectDir encodes a cwd absolute path into the directory name the
// Claude Code CLI uses under ~/.claude/projects: every path separator ('/')
// is replaced with '-' (including the leading one). absDir MUST be absolute
// (filepath.Abs-normalized) first — a relative path or trailing slash would
// encode to a different bucket than the CLI wrote, and the symptom is a
// silent "no sessions" (never a wrong-bucket delete), so encoding is safe by
// construction but should still be normalised by the caller.
func encodeProjectDir(absDir string) string {
	return strings.ReplaceAll(absDir, "/", "-")
}

// projectsDir resolves ~/.claude/projects, anchored at the same settingsDir
// the rest of the client uses (empty → ~/.claude via resolveSettingsDir).
func (c *Client) projectsDir() string {
	base := resolveSettingsDir(c.settingsDir)
	// resolveSettingsDir returns an absolute path when $HOME is set. When
	// $HOME is unset it returns its input verbatim (often ""), which is not a
	// real home — bail so the caller surfaces the misconfiguration rather than
	// writing under a bogus path.
	if base == "" || !filepath.IsAbs(base) {
		return ""
	}
	return filepath.Join(base, "projects")
}

// ListSessions enumerates the claude sessions whose cwd equals dir. It reads
// the filesystem directly instead of forking the CLI (there is no `claude
// session list` subcommand), so it is fast (sub-millisecond local I/O) and can
// be called synchronously from slash commands. dir MUST be absolute — encode
// divergence means the wrong bucket is read (no sessions), never a wrong one
// deleted. A non-existent projects dir yields (nil, nil): a chat whose cwd
// has never hosted a claude turn legitimately has zero sessions.
func (c *Client) ListSessions(_ context.Context, dir string) ([]Session, error) {
	root := c.projectsDir()
	if root == "" {
		return nil, errors.New("claude: cannot determine settings directory ($HOME unset)")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("claude: resolve dir: %w", err)
	}
	bucket := filepath.Join(root, encodeProjectDir(abs))
	entries, err := os.ReadDir(bucket)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("claude: read projects bucket: %w", err)
	}

	var out []Session
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !uuidRe.MatchString(id) {
			continue
		}
		path := filepath.Join(bucket, name)
		title := readSessionTitle(path)
		out = append(out, Session{
			ID:      id,
			Title:   title,
			Updated: fileModTimeMs(path),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}

// DeleteSession removes the session transcript (and its optional sidecar
// directory of blobs/sub-agents) whose id equals sessionID under dir. It is a
// filesystem operation only; the caller is responsible for confirming with
// the user and for never deleting the currently bound session (the bridge
// guards that at the handler layer). The project-level shared memory/ dir is
// NEVER touched. dir MUST be absolute and match the session's creation cwd.
func (c *Client) DeleteSession(_ context.Context, dir, sessionID string) error {
	if sessionID == "" {
		return errors.New("claude: session id is empty")
	}
	if !uuidRe.MatchString(sessionID) {
		return fmt.Errorf("claude: invalid session id %q", sessionID)
	}
	root := c.projectsDir()
	if root == "" {
		return errors.New("claude: cannot determine settings directory ($HOME unset)")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("claude: resolve dir: %w", err)
	}
	bucket := filepath.Join(root, encodeProjectDir(abs))
	jsonlPath := filepath.Join(bucket, sessionID+".jsonl")
	if err := os.Remove(jsonlPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("claude: session %s not found in %s", sessionID, abs)
		}
		return fmt.Errorf("claude: remove session file: %w", err)
	}
	// Best-effort sidecar removal (sub-agents / blobs). Failure is logged, not
	// fatal: the transcript is already gone, so the session is effectively
	// deleted even if a stray directory lingers.
	sidecar := filepath.Join(bucket, sessionID)
	if info, statErr := os.Stat(sidecar); statErr == nil && info.IsDir() {
		if rmErr := os.RemoveAll(sidecar); rmErr != nil {
			c.logger.Warn("claude: remove session sidecar", "path", sidecar, "error", rmErr)
		}
	}
	return nil
}

// readSessionTitle derives a display title from a session transcript's first
// user-facing record. The CLI writes no title field; the natural label is the
// opening prompt text. It scans the leading lines cheaply (a bufio scanner
// with a bounded buffer) and stops at the first usable line so reading a
// multi-MB transcript never happens. Returns "" when no usable line is found
// — the caller renders "(未命名会话)".
func readSessionTitle(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for lines := 0; lines < 40 && sc.Scan(); lines++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if title := extractTitleLine(line); title != "" {
			return truncateTitle(title)
		}
	}
	return ""
}

// sessionHeadRecord mirrors the line shapes the CLI writes at the head of a
// transcript. Two carry the opening prompt as "content": the initial
// queue-operation record (written first) and the first user message record.
// Either is a faithful title; whichever appears first wins.
type sessionHeadRecord struct {
	Type    string `json:"type"`
	Content any    `json:"content"`
}

// extractTitleLine parses one JSON line and returns its "content" when it is a
// plain string under a queue-operation or user record. Returns "" for any
// other shape (assistant text, tool blocks, array content, ...) so the caller
// keeps scanning.
func extractTitleLine(line string) string {
	var rec sessionHeadRecord
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		return ""
	}
	if rec.Type != "queue-operation" && rec.Type != "user" {
		return ""
	}
	if s, ok := rec.Content.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// truncateTitle caps a derived title to maxSessionTitleRunes, keeping the
// leading portion (the start of the prompt is the identifiable part) and
// suffixing an ellipsis.
func truncateTitle(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxSessionTitleRunes {
		return s
	}
	return string(r[:maxSessionTitleRunes]) + "…"
}

// fileModTimeMs returns a file's mtime as unix milliseconds, or 0 on error.
func fileModTimeMs(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}

// FormatSessionTime renders a millisecond timestamp as a relative string. It
// mirrors omp/opencode's bands so the /session-list and /use rows
// read the same shape across backends.
func FormatSessionTime(ms int64) string {
	if ms == 0 {
		return "(未知)"
	}
	t := time.UnixMilli(ms)
	diff := time.Since(t)
	switch {
	case diff < time.Minute:
		return "刚刚"
	case diff < time.Hour:
		return fmt.Sprintf("%d分钟前", int(diff.Minutes()))
	case diff < 24*time.Hour:
		hours := int(diff.Hours())
		if hours == 1 {
			return "1小时前"
		}
		return fmt.Sprintf("%d小时前", hours)
	case diff < 7*24*time.Hour:
		days := int(diff.Hours() / 24)
		if days == 1 {
			return "1天前"
		}
		return fmt.Sprintf("%d天前", days)
	default:
		return t.Format("2006-01-02 15:04")
	}
}
