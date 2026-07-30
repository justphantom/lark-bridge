// Package ompbridge glues Feishu events to the Oh My Pi (omp) CLI agent
// backend.
//
// One Handler per process owns the router (chatID -> omp session binding),
// the omp CLI client, and the backendrpc client used to emit Control
// messages. Like the opencode/claude bridges, this bridge is stream-driven:
// one `omp -p --mode json` subprocess per turn, whose NDJSON stream IS the
// response.
package ompbridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/omp"
)

// ompAPI is the omp backend capability the bridge needs. The production
// implementation is *omp.Client; the interface exists so handler tests can
// substitute a fake that replays canned event streams.
//
// Run drives agent turns; ListModels runs `omp models --json` for the
// interactive /model picker (provider catalog fetch, ~100-150s cold; cached
// in the client). ListSessions reads the omp session store from disk so
// /session-list, /session-use and /session-clean can be implemented without
// forking the slow CLI.
type ompAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event (agent_end /
	// error) precedes close.
	Run(ctx context.Context, opts omp.RunOptions) (<-chan omp.Event, error)
	// ListModels runs `omp models --json` for the interactive /model picker.
	// Returns one `provider/id` selector per model.
	ListModels(ctx context.Context) ([]string, error)
	// ListSessions enumerates omp sessions whose cwd equals dir. Empty dir
	// returns all sessions under the agent directory. It reads the filesystem
	// directly and is fast enough to call synchronously.
	ListSessions(ctx context.Context, dir string) ([]omp.Session, error)
	// DeleteSession removes the omp session file (and sidecar directory) whose
	// id equals id and whose cwd equals dir.
	DeleteSession(ctx context.Context, dir, id string) error
	// CleanSessions deletes every session whose cwd equals dir except keepID.
	CleanSessions(ctx context.Context, dir, keepID string) ([]string, error)
	// RunGC forks `omp gc --apply --archive --json` to reconcile the session
	// store and the history.db/FTS index.
	RunGC(ctx context.Context, opts omp.GCOptions) (omp.GCResult, error)
}
