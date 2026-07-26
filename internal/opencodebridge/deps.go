// Package opencodebridge glues Feishu events to the opencode CLI agent
// backend.
//
// One Handler per process owns the router (chatID -> opencode session
// binding), the opencode CLI client, and the backendrpc client used to emit
// Control messages. Like the claude bridge, this bridge is stream-driven:
// one `opencode run --format json --auto` subprocess per turn, whose NDJSON
// stream IS the response.
package opencodebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/opencode"
)

// opencodeAPI is the opencode backend capability the bridge needs. The
// production implementation is *opencode.Client; the interface exists so
// handler tests can substitute a fake that replays canned event streams.
type opencodeAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event (result/error)
	// precedes close.
	Run(ctx context.Context, opts opencode.RunOptions) (<-chan opencode.Event, error)
	// ListModels runs `opencode models` for the interactive /model picker.
	// Returns one `provider/model` entry per line.
	ListModels(ctx context.Context) ([]string, error)
	// ListAgents runs `opencode agent list` for the interactive /agent picker.
	// Returns user-visible agent names (hidden internal agents filtered).
	ListAgents(ctx context.Context) ([]string, error)
	// ListSessions runs `opencode session list --format json` scoped to dir.
	// The CLI's session store is cwd-bound: only sessions created under dir
	// are returned. Used by /session-list and /session-clean.
	ListSessions(ctx context.Context, dir string) ([]opencode.Session, error)
	// DeleteSession runs `opencode session delete <id>` scoped to dir. The
	// CLI's lookup is also cwd-bound. Used by /session-clean.
	DeleteSession(ctx context.Context, dir, sessionID string) error
}
