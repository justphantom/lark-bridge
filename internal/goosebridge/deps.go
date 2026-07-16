// Package goosebridge glues Feishu events to the goose CLI agent backend.
//
// One Handler per process owns the router (chatID -> per-chat working directory
// binding + goose session name), the goose CLI client, and the backendrpc
// client used to emit Control messages. Like claude-back this bridge is
// session-aware: goose keeps sessions in a global SQLite DB and is resumed by
// name. The router binding's SessionID stores the --name anchor
// ("feishu:<chatID>"); the first turn creates it (--name without --resume),
// subsequent turns resume it (--resume --name). A stale anchor (session DB
// reset) surfaces as a "No session found" error and is retried once without
// --resume, mirroring claude-back's stale-session recovery.
//
// goose ALWAYS exits 0, so success is judged by whether a complete event
// reached stdout (see internal/goose/client.go). The bridge forwards thinking
// deltas as TypeThinking and records token usage from the complete event.
package goosebridge

import (
	"context"

	"github.com/hu/lark-bridge/internal/goose"
	"github.com/hu/lark-bridge/internal/router"
)

// gooseAPI is the goose backend capability the bridge needs. The production
// implementation is *goose.Client; the interface exists so handler tests can
// substitute a fake that replays canned event streams.
type gooseAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event (complete/error)
	// precedes close.
	Run(ctx context.Context, opts goose.RunOptions) (<-chan goose.Event, error)
}

// sessionRouter is the subset of *router.Router the Handler uses. goose-back
// writes the --name anchor into SessionID (the bridge back-fills it on the
// first successful turn and resets it on /cd, /session-new). Directory,
// modelSpec, permissionMode, effortLevel, settingsFile persist across turns.
type sessionRouter interface {
	Lookup(chatID string) (router.Binding, bool)
	Bind(chatID, sessionID, directory, title, modelSpec, agent string)
	Unbind(chatID string)
	TitleOf(chatID string) string
	AllBindings() map[string]router.Binding
	SetSessionID(chatID, sessionID string)
	SetModelSpec(chatID, modelSpec string)
	SetDirectory(chatID, directory string)
	SetPermissionMode(chatID, permissionMode string)
	SetEffortLevel(chatID, effortLevel string)
	SetSettingsFile(chatID, settingsFile string)
}
