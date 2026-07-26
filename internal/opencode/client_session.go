package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

// sessionTimeout bounds `opencode session list` / `opencode session delete`.
// The CLI forks the same heavy provider/config load as `models`/`agent list`
// before the subcommand runs (observed 25–50s); 90s covers the worst seen
// while still bounding a hang. Reuses listTimeout's value rather than
// re-declaring the rationale.
const sessionTimeout = listTimeout

// Session is the subset of `opencode session list --format json` fields the
// bridge renders. The CLI emits more (projectId, directory) but only
// id/title/timestamps are shown to the user.
type Session struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Updated int64  `json:"updated"` // ms epoch
	Created int64  `json:"created"` // ms epoch
}

// ListSessions runs `opencode session list --format json` scoped to dir. The
// CLI's session store is cwd-bound: only sessions created under dir are
// returned, so the binding's directory MUST be passed for the list to match
// what that chat actually used. dir == "" runs in the process cwd.
//
// Not cached: clean operations need a fresh view and call frequency is low
// (only on /session-list), unlike models/agent pickers which fire on every
// /model or /agent.
func (c *Client) ListSessions(ctx context.Context, dir string) ([]Session, error) {
	if c.cliPath == "" {
		return nil, errors.New("opencode: cli_path is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()
	// #nosec G204 -- c.cliPath comes from the trusted config file; args are
	// fixed subcommand flags, not user input.
	cmd := exec.CommandContext(ctx, c.cliPath, "session", "list", "--format", "json")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode session list: %w", err)
	}
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	var sessions []Session
	if err := json.Unmarshal(out, &sessions); err != nil {
		return nil, fmt.Errorf("parse session list: %w", err)
	}
	return sessions, nil
}

// DeleteSession runs `opencode session delete <sessionID>` scoped to dir. The
// CLI's delete is also cwd-bound (it looks up the session under dir's
// project), so dir MUST match the directory the session was created in.
// Returns nil on success; on failure the error wraps both the CLI's exit
// error and its combined stdout/stderr so the red "Session not found" line
// surfaces verbatim.
func (c *Client) DeleteSession(ctx context.Context, dir, sessionID string) error {
	if c.cliPath == "" {
		return errors.New("opencode: cli_path is empty")
	}
	if sessionID == "" {
		return errors.New("opencode: session id is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, sessionTimeout)
	defer cancel()
	// #nosec G204 -- c.cliPath comes from the trusted config file; sessionID
	// is constrained to an ID returned by ListSessions (the slash command
	// validates it against the listing before reaching here).
	cmd := exec.CommandContext(ctx, c.cliPath, "session", "delete", sessionID)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if dir != "" {
		cmd.Dir = dir
	}
	// session delete writes its result to stdout on success and to stderr on
	// failure (both with ANSI colour codes); CombinedOutput captures both so
	// an error message includes the CLI's verbatim "Session not found: ...".
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("opencode session delete %s: %w (%s)", sessionID, err, strings.TrimSpace(string(out)))
	}
	return nil
}
