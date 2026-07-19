// Package miniagent is the bridge-side handler for the miniagent CLI
// subprocess. As of the bridge↔CLI single-source-of-truth refactor, this
// package no longer re-implements miniagent's state layer (sessions, facts,
// per-chat pins). Instead every state read/write is delegated to the CLI
// binary via its -show-current / -list-sessions / -set-* / -memory-*
// subcommands. The bridge therefore never reads .model/.dir/.perm/.cur or
// any jsonl/json state file directly — the CLI is the sole owner.
//
// The trade-off is ~5ms of fork overhead per state access. In practice the
// turn's LLM call dominates by 3-4 orders of magnitude, and slash commands
// are user-paced (sub-100ms feels instant). The win is eliminating an entire
// second copy of the state layout that used to drift out of sync with the
// CLI and caused real bugs (meta/ vs history/ migration, required:null
// schema, etc.).
package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Fact is one structured long-term memory entry. Mirrors miniagent.Fact on
// the CLI side; the bridge only reads these (never constructs them).
type Fact struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Scope     string    `json:"scope"`
	UpdatedAt time.Time `json:"-"`
	// RawDate is the formatted timestamp from the CLI (already a string);
	// callers that just echo it back to the user use this field directly.
	RawDate string `json:"-"`
}

// SessionInfo describes one stored session of a chat.
type SessionInfo struct {
	ID      string
	Bytes   int64
	ModTime time.Time
	Current bool
}

// CurrentState is the snapshot returned by -show-current. Every field is
// sourced from the CLI's MetaStore; the bridge never reads the underlying
// .model / .dir / .perm files.
type CurrentState struct {
	ChatID     string
	SessionID  string
	Model      string
	Directory  string
	Permission string
}

// CLIState wraps the miniagent binary path + state-dir, providing typed
// methods for every subcommand the bridge needs. Each method forks the CLI
// once, parses its JSON output, and returns the typed result. Errors from
// the CLI (exit code != 0, malformed JSON, etc.) are returned as Go errors.
//
// The bridge constructs one CLIState at startup (cmd/miniagent-back/main.go)
// and shares it across all goroutines — every method is stateless and safe
// for concurrent use.
type CLIState struct {
	binary   string
	stateDir string
	apiKey   string // injected as $MINIAGENT_API_KEY on every fork (-list-models needs it)
	baseURL  string // injected as $MINIAGENT_BASE_URL on every fork (-list-models needs it)
}

// NewCLIState builds a CLIState. binary is the miniagent executable path;
// stateDir is the -state-dir passed to every subcommand. apiKey/baseURL are
// set on each subprocess env so subcommands that talk to the LLM endpoint
// (notably -list-models) work even when the backend's own env lacks them —
// mirroring how miniclient.Client.Run injects the key for the main flow.
func NewCLIState(binary, stateDir, apiKey, baseURL string) *CLIState {
	return &CLIState{binary: binary, stateDir: stateDir, apiKey: apiKey, baseURL: baseURL}
}

// envForFork returns the env list for a CLI subprocess: the parent env plus
// MINIAGENT_API_KEY / MINIAGENT_BASE_URL overridden from config. We set them
// unconditionally (empty is fine for state-only subcommands) so -list-models
// does not silently inherit a stale or missing key.
func (c *CLIState) envForFork() []string {
	return append(os.Environ(),
		"MINIAGENT_API_KEY="+c.apiKey,
		"MINIAGENT_BASE_URL="+c.baseURL,
	)
}

// call runs the CLI with the given extra args and returns stdout. stderr is
// captured into the error for diagnostics. ctx bounds the fork.
func (c *CLIState) call(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"-state-dir", c.stateDir}, args...)
	// #nosec G204 -- c.binary is trusted config; args are built internally.
	cmd := exec.CommandContext(ctx, c.binary, full...)
	cmd.Env = c.envForFork()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("miniagent %v: %w: %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// ShowCurrent returns the per-chat snapshot (session/model/dir/permission).
// Called once per turn (handler_cli.go) to decide the CLI subprocess flags.
func (c *CLIState) ShowCurrent(ctx context.Context, chatID string) (CurrentState, error) {
	out, err := c.call(ctx, "-chat-id", chatID, "-show-current")
	if err != nil {
		return CurrentState{}, err
	}
	var raw struct {
		ChatID     string `json:"chat_id"`
		SessionID  string `json:"session_id"`
		Model      string `json:"model"`
		Directory  string `json:"directory"`
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return CurrentState{}, fmt.Errorf("parse show-current: %w (raw: %s)", err, string(out))
	}
	return CurrentState{
		ChatID:     raw.ChatID,
		SessionID:  raw.SessionID,
		Model:      raw.Model,
		Directory:  raw.Directory,
		Permission: raw.Permission,
	}, nil
}

// ListSessions returns the chat's stored sessions (oldest first, current
// marked). Empty list when the chat has no sessions yet.
func (c *CLIState) ListSessions(ctx context.Context, chatID string) ([]SessionInfo, error) {
	out, err := c.call(ctx, "-chat-id", chatID, "-list-sessions")
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID      string `json:"id"`
		Current bool   `json:"current"`
		Bytes   int64  `json:"bytes"`
		ModTime string `json:"mod_time"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse list-sessions: %w (raw: %s)", err, string(out))
	}
	out2 := make([]SessionInfo, 0, len(raw))
	for _, r := range raw {
		t, _ := time.Parse("2006-01-02 15:04:05", r.ModTime)
		out2 = append(out2, SessionInfo{ID: r.ID, Current: r.Current, Bytes: r.Bytes, ModTime: t})
	}
	return out2, nil
}

// NewSession creates a fresh session and returns its id. The previous
// session stays on disk.
func (c *CLIState) NewSession(ctx context.Context, chatID string) (string, error) {
	out, err := c.call(ctx, "-chat-id", chatID, "-new-session")
	if err != nil {
		return "", err
	}
	var raw struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", fmt.Errorf("parse new-session: %w (raw: %s)", err, string(out))
	}
	return raw.SessionID, nil
}

// UseSession switches the chat's current session pointer.
func (c *CLIState) UseSession(ctx context.Context, chatID, sid string) error {
	_, err := c.call(ctx, "-chat-id", chatID, "-use-session", sid)
	return err
}

// DeleteSession removes a session (the active one when sid is empty).
func (c *CLIState) DeleteSession(ctx context.Context, chatID, sid string) error {
	target := sid
	if target == "" {
		// The CLI's -del-session requires a non-empty id; emulate the bridge's
		// historical "delete current" convenience by looking it up first.
		cur, err := c.ShowCurrent(ctx, chatID)
		if err != nil {
			return err
		}
		if cur.SessionID == "" {
			return fmt.Errorf("no active session to delete")
		}
		target = cur.SessionID
	}
	_, err := c.call(ctx, "-chat-id", chatID, "-del-session", target)
	return err
}

// SetModel pins (or clears when value is empty) the chat's model. Clear
// uses the CLI's explicit -clear-model flag rather than passing empty to
// -set-model, because -model has no sentinel and the bridge wants the
// semantics of "empty always means clear" without flag-parser ambiguity.
func (c *CLIState) SetModel(ctx context.Context, chatID, model string) error {
	args := []string{"-chat-id", chatID}
	if model == "" {
		args = append(args, "-clear-model")
	} else {
		args = append(args, "-set-model", "-model", model)
	}
	_, err := c.call(ctx, args...)
	return err
}

// SetDir pins (or clears when value is empty) the chat's working directory.
func (c *CLIState) SetDir(ctx context.Context, chatID, dir string) error {
	args := []string{"-chat-id", chatID}
	if dir == "" {
		args = append(args, "-clear-dir")
	} else {
		args = append(args, "-set-dir", "-workdir", dir)
	}
	_, err := c.call(ctx, args...)
	return err
}

// SetPermission pins (or clears when value is empty) the chat's permission.
func (c *CLIState) SetPermission(ctx context.Context, chatID, perm string) error {
	args := []string{"-chat-id", chatID}
	if perm == "" {
		args = append(args, "-clear-permission")
	} else {
		args = append(args, "-set-permission", "-permission", perm)
	}
	_, err := c.call(ctx, args...)
	return err
}

// ListFacts returns the chat's long-term facts, optionally filtered by key
// prefix. Returns an empty (non-nil) slice when none match.
func (c *CLIState) ListFacts(ctx context.Context, chatID, prefix string) ([]Fact, error) {
	args := []string{"-chat-id", chatID, "-memory-list"}
	if prefix != "" {
		args = append(args, "-prefix", prefix)
	}
	out, err := c.call(ctx, args...)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		Scope     string `json:"scope"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse memory-list: %w (raw: %s)", err, string(out))
	}
	out2 := make([]Fact, 0, len(raw))
	for _, r := range raw {
		t, _ := time.Parse("2006-01-02 15:04:05", r.UpdatedAt)
		out2 = append(out2, Fact{Key: r.Key, Value: r.Value, Scope: r.Scope, UpdatedAt: t, RawDate: r.UpdatedAt})
	}
	return out2, nil
}

// DeleteFact removes one fact. Returns whether the key existed.
func (c *CLIState) DeleteFact(ctx context.Context, chatID, key string) (bool, error) {
	out, err := c.call(ctx, "-chat-id", chatID, "-memory-delete", key)
	if err != nil {
		return false, err
	}
	var raw struct {
		Existed bool `json:"existed"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return false, fmt.Errorf("parse memory-delete: %w (raw: %s)", err, string(out))
	}
	return raw.Existed, nil
}

// SearchFacts returns facts whose key or value contains query
// (case-insensitive). Limited to 20 results by the CLI.
func (c *CLIState) SearchFacts(ctx context.Context, chatID, query string) ([]Fact, error) {
	out, err := c.call(ctx, "-chat-id", chatID, "-memory-search", query)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		Scope     string `json:"scope"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse memory-search: %w (raw: %s)", err, string(out))
	}
	out2 := make([]Fact, 0, len(raw))
	for _, r := range raw {
		t, _ := time.Parse("2006-01-02 15:04:05", r.UpdatedAt)
		out2 = append(out2, Fact{Key: r.Key, Value: r.Value, Scope: r.Scope, UpdatedAt: t, RawDate: r.UpdatedAt})
	}
	return out2, nil
}

// ListModels calls the CLI's -list-models subcommand, which in turn calls
// the LLM endpoint's GET /v1/models. The bridge uses this for /models and
// the /model picker. Kept here (rather than on a separate HTTP client) so
// the bridge carries zero HTTP/LLM code — the CLI is the only outbound
// HTTP surface.
func (c *CLIState) ListModels(ctx context.Context) ([]string, error) {
	// -list-models does not need -chat-id or -state-dir, but does need
	// $MINIAGENT_API_KEY and $MINIAGENT_BASE_URL; callNoStateDir injects both
	// from config rather than relying on the parent env being set.
	out, err := c.callNoStateDir(ctx, "-list-models")
	if err != nil {
		return nil, err
	}
	var models []string
	if err := json.Unmarshal(out, &models); err != nil {
		return nil, fmt.Errorf("parse list-models: %w (raw: %s)", err, string(out))
	}
	return models, nil
}

// callNoStateDir runs the CLI without -state-dir (for subcommands like
// -list-models that do not touch state).
func (c *CLIState) callNoStateDir(ctx context.Context, args ...string) ([]byte, error) {
	// #nosec G204 -- c.binary is trusted config.
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Env = c.envForFork()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("miniagent %v: %w: %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
