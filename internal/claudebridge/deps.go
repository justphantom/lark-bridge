package claudebridge

import (
	"context"

	"github.com/hu/lark-bridge/internal/claude"
	"github.com/hu/lark-bridge/internal/router"
)

// claudeAPI is the Claude backend capability the bridge needs. The
// production implementation is *claude.Client; the interface exists so
// handler tests can substitute a fake that replays canned event streams.
type claudeAPI interface {
	// Run starts one agent turn and returns the event stream. The caller
	// drains the channel until it is closed; a terminal event
	// (result/error) precedes close.
	Run(ctx context.Context, opts claude.RunOptions) (<-chan claude.Event, error)
	// ListSettings returns absolute paths of settings files in the settings
	// directory, for the interactive /settings picker. Cached per config.
	ListSettings(ctx context.Context) ([]string, error)
}

// sessionRouter is the subset of *router.Router the Handler uses. The
// merged router's Bind is the 6-arg superset (agent); claude-back passes
// "" for agent. SetSessionID back-fills the lazily-captured session id
// after a run's system/init event.
type sessionRouter interface {
	Lookup(chatID string) (router.Binding, bool)
	Bind(chatID, sessionID, directory, title, modelSpec, agent string)
	Unbind(chatID string)
	TitleOf(chatID string) string
	SetModelSpec(chatID, modelSpec string)
	SetSessionID(chatID, sessionID string)
	SetDirectory(chatID, directory string)
	SetPermissionMode(chatID, permissionMode string)
	SetEffortLevel(chatID, effortLevel string)
	SetSettingsFile(chatID, settingsFile string)
	AllBindings() map[string]router.Binding
}
