package opencodeserve

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/justphantom/lark-bridge/internal/strutil"
)

// Sentinel errors let callers branch on common failure modes with errors.Is
// instead of string-matching the formatted HTTP error. Each wraps the server
// response detail so the original message is still visible via Error().
var (
	// ErrNotFound: the opencode serve resource (session, message, ...) does
	// not exist — typically HTTP 404. A common cause is a persisted session
	// id that the server has garbage-collected after a restart.
	ErrNotFound = errors.New("opencodeserve: not found")
	// ErrSessionBusy: the session is already running a turn — HTTP 409.
	// Callers should abort or wait rather than retry immediately.
	ErrSessionBusy = errors.New("opencodeserve: session busy")
	// ErrUnauthorized: authentication failed or expired — HTTP 401.
	ErrUnauthorized = errors.New("opencodeserve: unauthorized")
)

// apiError maps a 4xx/5xx response body onto a sentinel error when
// recognisable, otherwise a generic HTTP error carrying the truncated body.
// detail is the already-read (and trimmed) response body, if any.
func apiError(status int, detail string) error {
	switch status {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, detail)
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, detail)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrSessionBusy, detail)
	default:
		return fmt.Errorf("opencodeserve: HTTP %d: %s", status, detail)
	}
}

// truncateDetail trims a response body to a diagnostic-friendly length.
func truncateDetail(body []byte) string {
	return strutil.Truncate(strings.TrimSpace(string(body)), 200)
}
