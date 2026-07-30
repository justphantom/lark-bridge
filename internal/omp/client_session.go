//go:build linux || darwin

package omp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// Session is the subset of an omp session file header that the bridge renders.
// omp stores sessions as {agentDir}/sessions/<encoded-cwd>/<base>.jsonl plus
// an optional <base> sidecar directory for blobs. The header lines carry the
// title and the absolute cwd; we read only those two lines.
type Session struct {
	ID      string
	Title   string
	Cwd     string
	Updated int64 // unix ms
}

// defaultAgentDir returns the omp default agent directory. The CLI uses
// ~/.omp/agent unless PI_CODING_AGENT_DIR or --session-dir overrides it.
// The bridge currently does not pass --session-dir, so this matches production.
func defaultAgentDir() string {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".omp", "agent")
}

// ListSessions enumerates omp sessions whose cwd equals dir. If dir is empty,
// all sessions under the agent directory are returned. It reads the filesystem
// directly instead of forking the CLI, so it is fast (milliseconds) and can be
// called synchronously from slash commands.
func (c *Client) ListSessions(ctx context.Context, dir string) ([]Session, error) {
	root := c.agentDir
	if root == "" {
		root = defaultAgentDir()
	}
	if root == "" {
		return nil, errors.New("omp: cannot determine agent directory (no home dir)")
	}
	sessionsDir := filepath.Join(root, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("omp: read sessions dir: %w", err)
	}

	var out []Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(sessionsDir, e.Name())
		subs, err := os.ReadDir(child)
		if err != nil {
			c.logger.Warn("omp: list session subdir", "dir", child, "error", err)
			continue
		}
		for _, sub := range subs {
			if sub.IsDir() {
				continue
			}
			if !strings.HasSuffix(sub.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(child, sub.Name())
			sess, ok, err := readSessionHeader(path)
			if err != nil {
				c.logger.Warn("omp: read session header", "path", path, "error", err)
				continue
			}
			if !ok {
				continue
			}
			if dir != "" && sess.Cwd != dir {
				continue
			}
			out = append(out, sess)
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out, nil
}

// sessionHeaderTitle mirrors the first line of an omp session .jsonl.
type sessionHeaderTitle struct {
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}

// sessionHeaderSession mirrors the second line of an omp session .jsonl.
type sessionHeaderSession struct {
	ID  string `json:"id"`
	Cwd string `json:"cwd"`
}

// readSessionHeader parses the first two lines of a session .jsonl file.
// It returns (session, true) when both header lines are present and valid.
func readSessionHeader(path string) (Session, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return Session{}, false, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)

	var titleLine, sessionLine string
	if sc.Scan() {
		titleLine = sc.Text()
	}
	if sc.Scan() {
		sessionLine = sc.Text()
	}
	if err := sc.Err(); err != nil {
		return Session{}, false, err
	}
	if titleLine == "" || sessionLine == "" {
		return Session{}, false, nil
	}

	var title sessionHeaderTitle
	if err := json.Unmarshal([]byte(titleLine), &title); err != nil {
		return Session{}, false, nil // tolerant: skip malformed files
	}
	var sess sessionHeaderSession
	if err := json.Unmarshal([]byte(sessionLine), &sess); err != nil {
		return Session{}, false, nil
	}
	if sess.ID == "" {
		return Session{}, false, nil
	}

	updated := fileModTimeMs(path)
	if t, err := time.Parse(time.RFC3339, title.UpdatedAt); err == nil {
		updated = t.UnixMilli()
	}

	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	id := sess.ID
	if id == "" {
		id = base
	}

	return Session{
		ID:      id,
		Title:   title.Title,
		Cwd:     sess.Cwd,
		Updated: updated,
	}, true, nil
}

// DeleteSession removes the omp session file (and its sidecar directory) whose
// id equals id and whose cwd equals dir. It is a filesystem operation only; the
// caller is responsible for confirming with the user and for updating bindings.
// history.db is NOT updated here — /session-gc reconciles the index later.
func (c *Client) DeleteSession(_ context.Context, dir, id string) error {
	root := c.agentDir
	if root == "" {
		root = defaultAgentDir()
	}
	if root == "" {
		return errors.New("omp: cannot determine agent directory (no home dir)")
	}
	sessionsDir := filepath.Join(root, "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return fmt.Errorf("omp: read sessions dir: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		child := filepath.Join(sessionsDir, e.Name())
		subs, err := os.ReadDir(child)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if sub.IsDir() {
				continue
			}
			if !strings.HasSuffix(sub.Name(), ".jsonl") {
				continue
			}
			path := filepath.Join(child, sub.Name())
			sess, ok, err := readSessionHeader(path)
			if err != nil || !ok {
				continue
			}
			if sess.Cwd != dir || sess.ID != id {
				continue
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("omp: remove session file: %w", err)
			}
			sidecar := filepath.Join(child, strings.TrimSuffix(sub.Name(), ".jsonl"))
			if info, err := os.Stat(sidecar); err == nil && info.IsDir() {
				if err := os.RemoveAll(sidecar); err != nil {
					c.logger.Warn("omp: remove session sidecar", "path", sidecar, "error", err)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("omp: session %s not found in %s", id, dir)
}

// CleanSessions deletes every omp session whose cwd equals dir except the one
// with id keepID. It returns the ids that were actually deleted. If keepID is
// empty, all sessions in dir are deleted.
func (c *Client) CleanSessions(ctx context.Context, dir, keepID string) ([]string, error) {
	sessions, err := c.ListSessions(ctx, dir)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, s := range sessions {
		if s.ID == keepID {
			continue
		}
		if err := c.DeleteSession(ctx, dir, s.ID); err != nil {
			c.logger.Warn("omp: clean session failed",
				"session_id", s.ID,
				"directory", dir,
				"error", err)
			continue
		}
		deleted = append(deleted, s.ID)
	}
	return deleted, nil
}

// GCOptions configures a RunGC invocation.
type GCOptions struct {
	// AgentDir overrides the agent directory. Empty uses the Client's AgentDir
	// or omp's default.
	AgentDir string
	// ColdArchiveAfterDays maps to --cold-archive-after-days.
	ColdArchiveAfterDays int
	// RetainNewestPerCwd maps to --retain-newest-per-cwd.
	RetainNewestPerCwd int
	// Timeout bounds the fork. 0 defaults to 5 minutes.
	Timeout time.Duration
}

// GCResult is the subset of `omp gc --json` output that /session-gc renders.
type GCResult struct {
	AgentDir           string
	Archived           int
	KeptNewestGlobal   int
	KeptNewestPerCwd   int
	Scanned            int
	SkippedActive      int
	HistoryRowsDeleted int
	FTSRebuilt         bool
	Errors             []string
}

// RunGC forks `omp gc --apply --archive --json` to reconcile the session store
// and history.db/FTS index. It is the correct cleanup path after /session-clean.
func (c *Client) RunGC(ctx context.Context, opts GCOptions) (GCResult, error) {
	agentDir := opts.AgentDir
	if agentDir == "" {
		agentDir = c.agentDir
	}
	if agentDir == "" {
		agentDir = defaultAgentDir()
	}
	if agentDir == "" {
		return GCResult{}, errors.New("omp: cannot determine agent directory (no home dir)")
	}
	if opts.ColdArchiveAfterDays < 0 {
		opts.ColdArchiveAfterDays = 30
	}
	if opts.RetainNewestPerCwd < 0 {
		opts.RetainNewestPerCwd = 5
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{
		"gc",
		"--apply",
		"--archive",
		"--json",
		"--agent-dir", agentDir,
		"--cold-archive-after-days", strconv.Itoa(opts.ColdArchiveAfterDays),
		"--retain-newest-per-cwd", strconv.Itoa(opts.RetainNewestPerCwd),
	}
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	cmd.Env = cmdutil.SanitizeChildEnv()
	out, err := cmd.Output()
	if err != nil {
		return GCResult{}, fmt.Errorf("omp gc: %w", err)
	}

	var raw gcResultJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return GCResult{}, fmt.Errorf("omp gc: parse output: %w", err)
	}

	var errs []string
	for _, e := range raw.Archive.Errors {
		errs = append(errs, e)
	}

	return GCResult{
		AgentDir:           raw.AgentDir,
		Archived:           raw.Archive.Archived,
		KeptNewestGlobal:   raw.Archive.KeptNewestGlobal,
		KeptNewestPerCwd:   raw.Archive.KeptNewestPerCwd,
		Scanned:            raw.Archive.Scanned,
		SkippedActive:      raw.Archive.SkippedActive,
		HistoryRowsDeleted: raw.Archive.HistoryRowsDeleted,
		FTSRebuilt:         raw.Archive.FTSRebuilt,
		Errors:             errs,
	}, nil
}

// gcResultJSON mirrors the `omp gc --json` payload (single-line object).
type gcResultJSON struct {
	AgentDir string `json:"agentDir"`
	Archive  struct {
		Scanned            int      `json:"scanned"`
		SkippedActive      int      `json:"skippedActive"`
		KeptNewestGlobal   int      `json:"keptNewestGlobal"`
		KeptNewestPerCwd   int      `json:"keptNewestPerCwd"`
		Archived           int      `json:"archived"`
		HistoryRowsDeleted int      `json:"historyRowsDeleted"`
		FTSRebuilt         bool     `json:"ftsRebuilt"`
		Errors             []string `json:"errors"`
	} `json:"archive"`
}

func fileModTimeMs(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixMilli()
}
