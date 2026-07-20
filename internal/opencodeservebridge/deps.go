// Package opencodeservebridge glues Feishu events to a running opencode serve
// HTTP server.
//
// One Handler per process owns the router (chatID -> opencode session
// binding), the opencode serve client, and the backendrpc client used to
// emit Control messages. One async session message per turn; events arrive
// over the global /event SSE stream.
package opencodeservebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/opencodeserve"
)

// opencodeAPI is the opencode backend capability the bridge needs. The
// production implementation is *opencodeserve.Client; the interface exists so
// handler tests can substitute a fake that replays canned event streams.
type opencodeAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event (result/error)
	// precedes close.
	Run(ctx context.Context, opts opencodeserve.RunOptions) (<-chan opencodeserve.Event, error)
	// ListModels queries the serve catalog for the interactive /model picker.
	// Returns one "provider/model" entry per active model.
	ListModels(ctx context.Context) ([]string, error)
	// ListAgents queries the serve catalog for the interactive /agent picker.
	// Returns user-visible agent ids (hidden internal agents filtered).
	ListAgents(ctx context.Context) ([]string, error)
	// AbortSession POSTs /session/{id}/abort on the serve server. Unlike CLI
	// mode, a stuck 'busy' session lives server-side and is not released by
	// cancelling the local pump context — the slash command must call this
	// to recover from a wedged turn.
	AbortSession(ctx context.Context, sessionID string) error
}
