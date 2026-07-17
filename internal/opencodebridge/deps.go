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

	"github.com/hu/lark-bridge/internal/opencode"
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
}
