package claudebridge

import (
	"context"

	"github.com/justphantom/lark-bridge/internal/claude"
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
	// ListSessions enumerates the claude sessions under dir (filesystem scan
	// of ~/.claude/projects/<encoded-cwd>). dir MUST be absolute; a relative
	// path encodes to a different bucket and yields nothing. Used by
	// /session-list, /session-clean and /session-use.
	ListSessions(ctx context.Context, dir string) ([]claude.Session, error)
	// DeleteSession removes the session transcript (and optional sidecar dir)
	// whose id equals sessionID under dir. The project-level shared memory/
	// dir is never touched. Used by /session-clean. The bridge guards the
	// currently bound session at the handler layer.
	DeleteSession(ctx context.Context, dir, sessionID string) error
}
