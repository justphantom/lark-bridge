// Package opencodeservebridge glues Feishu events to a running opencode serve
// HTTP server via the opencode-go-sdk-lite SDK.
//
// One Handler per process owns the router (chatID -> opencode session
// binding), the SDK-backed agent, and the backendrpc client used to emit
// Control messages. One Run per turn; HighEvents arrive over the SDK's
// global event stream.
package opencodeservebridge

import (
	"context"

	oc "github.com/justphantom/opencode-go-sdk-lite"
)

// opencodeAPI is the opencode backend capability the bridge needs. The
// production implementation is *Agent; the interface exists so handler tests
// can substitute a fake that replays canned HighEvent streams.
type opencodeAPI interface {
	// Run starts one agent turn and returns the HighEvent stream. The caller
	// drains the channel until it is closed; a terminal event (result/error)
	// precedes close.
	Run(ctx context.Context, opts oc.RunOptions) (<-chan oc.HighEvent, error)
	// ListModels queries the serve catalog for the interactive /model picker.
	// Returns one "provider/model" entry per active model.
	ListModels(ctx context.Context) ([]string, error)
	// ListAgents queries the serve catalog for the interactive /agent picker.
	// Returns user-visible agent ids (hidden internal agents filtered).
	ListAgents(ctx context.Context) ([]string, error)
	// AbortSession POSTs /api/session/{id}/interrupt on the serve server.
	// Required even when the bridge believes no local turn is running: a
	// stuck server-side 'busy' session is not released by cancelling the
	// local ctx.
	AbortSession(ctx context.Context, sessionID string) error
	// SwitchModel POSTs /api/session/{id}/model; spec is "provider/model"
	// or empty (clears the pin).
	SwitchModel(ctx context.Context, sessionID, spec string) error
	// SwitchAgent POSTs /api/session/{id}/agent.
	SwitchAgent(ctx context.Context, sessionID, agent string) error
}
