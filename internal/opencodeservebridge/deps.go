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
	// AbortSession POSTs /session/{id}/abort on the serve server.
	// Required even when the bridge believes no local turn is running: a
	// stuck server-side 'busy' session is not released by cancelling the
	// local ctx.
	AbortSession(ctx context.Context, sessionID string) error
	// ListSessions returns all sessions from the serve server.
	ListSessions(ctx context.Context) ([]oc.SessionInfo, error)
	// SessionStatuses returns the status map of all sessions.
	SessionStatuses(ctx context.Context) (map[string]oc.SessionStatus, error)
	// DeleteSessionIfIdle deletes a session only if it is idle.
	DeleteSessionIfIdle(ctx context.Context, sessionID string) error
	// ReplyPermission answers a pending permission request (once/always/reject).
	ReplyPermission(ctx context.Context, requestID, reply, message string) error
	// ReplyQuestion answers a pending question request; RejectQuestion
	// declines it. One of the two MUST eventually be called for every
	// question.asked event or the serve-side agent hangs forever.
	ReplyQuestion(ctx context.Context, requestID string, r *oc.QuestionReply) error
	RejectQuestion(ctx context.Context, requestID string) error
}
