package claudebridge

import (
	"context"

	"github.com/hu/lark-bridge/internal/claude"
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
