// Package peribridge glues Feishu events to the peri CLI agent backend.
//
// One Handler per process owns the router (chatID -> per-chat working
// directory binding), the peri CLI client, and the backendrpc client used to
// emit Control messages. Like the opencode bridge, this bridge is
// stream-driven: one `peri -p --output-format stream-json` subprocess per
// turn, whose NDJSON stream IS the response.
//
// Unlike opencode, peri print mode is stateless: there is no session id to
// capture or resume, so every turn starts fresh. The router binding still
// records the per-chat directory (so /cd persists across turns) and the
// modelSpec, but SessionID is always empty.
package peribridge

import (
	"context"

	"github.com/hu/lark-bridge/internal/peri"
)

// periAPI is the peri backend capability the bridge needs. The production
// implementation is *peri.Client; the interface exists so handler tests can
// substitute a fake that replays canned event streams.
type periAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event (result/error)
	// precedes close.
	Run(ctx context.Context, opts peri.RunOptions) (<-chan peri.Event, error)
}
