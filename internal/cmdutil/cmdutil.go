// Package cmdutil holds the slash-command infrastructure shared by the
// claude and opencode bridges: parsing, the timeout cap, and the pure
// helpers (error-result pairing, setting-change logging, help rendering)
// that both bridges used to duplicate verbatim.
//
// What stays in each bridge: the dispatcher (it binds the bridge's own
// *Handler for emit/logger), the per-backend command registry, and the
// per-backend handler implementations.
package cmdutil

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/log"
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
type Result struct {
	Body    string
	Field   string
	Before  string
	After   string
	Handled bool
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

// ErrorResult returns both a Result and an error carrying the same message.
// The message is formatted once so a percent sign in an argument cannot make
// the body and error disagree, and the error avoids a non-constant
// fmt.Errorf format string.
func ErrorResult(format string, args ...any) (Result, error) {
	msg := fmt.Sprintf(format, args...)
	return Result{Body: msg}, errors.New(msg)
}

// ChangeResult builds a Result for a setting-change command (Field/Before/After
// carry the structured change). body is the human confirmation line shown
// below the before→after block. before may be empty when there was no prior
// value (first-time set); the renderer then omits the strikethrough half.
func ChangeResult(field, before, after, body string) Result {
	return Result{Body: body, Field: field, Before: before, After: after}
}

// LogSettingChange records a setting change at info level. An empty value
// logs "<field> cleared"; otherwise "<field> set" plus the field=value pair.
func LogSettingChange(logger *log.Logger, chatID, field, value string) {
	if value == "" {
		logger.Info(fmt.Sprintf("%s cleared", field), log.FieldChatID, chatID)
	} else {
		logger.Info(fmt.Sprintf("%s set", field),
			log.FieldChatID, chatID,
			field, value)
	}
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
