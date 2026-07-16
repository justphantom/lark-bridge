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
	"github.com/hu/lark-bridge/internal/router"
)

// periAPI is the peri backend capability the bridge needs. The production
// implementation is *peri.Client; the interface exists so handler tests can
// substitute a fake that replays canned event streams.
type periAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event (result/error)
	// precedes close.
	Run(ctx context.Context, opts peri.RunOptions) (<-chan peri.Event, error)
	// IsReady is the startup health gate (peri --version).
	IsReady(ctx context.Context) error
}

// sessionRouter is the subset of *router.Router the Handler uses. peri-back
// never reads SessionID (stateless), but still writes directory/modelSpec/
// permissionMode/effortLevel/settingsFile via the binding so /cd, /model,
// /perm, /effort, /settings persist across turns. SessionID-related methods
// are kept off this interface intentionally.
type sessionRouter interface {
	Lookup(chatID string) (router.Binding, bool)
	Bind(chatID, sessionID, directory, title, modelSpec, agent string)
	Unbind(chatID string)
	TitleOf(chatID string) string
	AllBindings() map[string]router.Binding
	SetModelSpec(chatID, modelSpec string)
	SetDirectory(chatID, directory string)
	SetPermissionMode(chatID, permissionMode string)
	SetEffortLevel(chatID, effortLevel string)
	SetSettingsFile(chatID, settingsFile string)
}
