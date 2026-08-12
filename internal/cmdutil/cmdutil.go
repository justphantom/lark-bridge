// Package cmdutil holds the slash-command infrastructure shared by the
// backends: parsing, the timeout cap, and the pure helpers (help rendering)
// that backends used to duplicate verbatim.
//
// What stays in each backend: the dispatcher (it binds the backend's own
// *Handler for emit/logger), the per-backend command registry, and the
// per-backend handler implementations.
package cmdutil

import (
	"strings"
	"time"
)

// Timeout caps how long any single slash command may take. Commands run in
// a goroutine, so without a bound a slow path would leak.
const Timeout = 15 * time.Second

// Result is the body a slash command returns. The dispatcher wraps it in a
// TypeNotice Control; a non-nil error is converted into an error-level
// notice instead. Field/Before/After carry a structured setting change: when
// Field is non-empty the notice renders a before→after block above Body so the
// user sees what moved, not just the new value.
//
// Handled, when true, signals that the handler has already emitted its own
// controls (e.g. an interactive question card that the user answers later)
// and the dispatcher must skip the default TypeNotice reply. The dispatcher
// still owns error handling: a non-nil error overrides Handled.
//
// Level and Title, when non-empty, override the values from the command's
// Spec for this invocation. This lets one command produce both success and
// warning/error replies without needing a separate spec per outcome.
type Result struct {
	Body    string
	Field   string
	Before  string
	After   string
	Handled bool
	Level   string // overrides Spec.Level if non-empty
	Title   string // overrides Spec.Title if non-empty
}

// Spec is one slash command's display metadata: the /help entry and the
// notice title/level the dispatcher applies to its reply. Each bridge wraps
// this in its own commandSpec, adding a Handler bound to its *Handler.
type Spec struct {
	Name    string
	Summary string
	Args    string
	Title   string
	Level   string // "info" | "success" | "warning" | "error"
}

// ParseCommand splits "/model claude-sonnet-4-5" → ("/model",
// ["claude-sonnet-4-5"]). A prompt not starting with "/" returns ("", nil).
func ParseCommand(prompt string) (cmd string, args []string) {
	parts := strings.SplitN(strings.TrimSpace(prompt), " ", 2)
	head := parts[0]
	rest := ""
	if len(parts) > 1 {
		rest = parts[1]
	}
	if !strings.HasPrefix(head, "/") {
		return "", nil
	}
	return head, strings.Fields(rest)
}

// RenderHelp renders the /help body from a list of specs, one line each:
//
//	/name [args] — summary
func RenderHelp(specs []Spec) string {
	var b strings.Builder
	b.WriteString("命令：\n")
	for _, s := range specs {
		b.WriteString(s.Name)
		if s.Args != "" {
			b.WriteByte(' ')
			b.WriteString(s.Args)
		}
		b.WriteString(" — ")
		b.WriteString(s.Summary)
		b.WriteByte('\n')
	}
	return b.String()
}
