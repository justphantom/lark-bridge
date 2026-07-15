// Package log provides a structured logger wrapper around log/slog.
//
// It offers a convenient API for creating slog loggers with component tags
// and supports both text and JSON output formats.
//
// Log level usage guidelines:
//
//   - Debug: Development and debugging information, should not appear in production
//   - Info:  Important nodes in normal business flow (binding creation, request completion)
//   - Warn:  Potential issues that need attention but do not affect service (retryable errors)
//   - Error: Error conditions that affect functionality but service can continue
package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"
)

// Aliases below re-export slog symbols so callers import only this
// package. The indirection is intentional: it keeps a single import
// site and lets us swap the underlying logger without touching every
// call site.

// Logger is slog.Logger alias for convenience.
type Logger = slog.Logger

// Level is slog.Level alias.
type Level = slog.Level

const (
	LevelDebug = slog.LevelDebug
	LevelInfo  = slog.LevelInfo
	LevelWarn  = slog.LevelWarn
	LevelError = slog.LevelError
)

// Standard log field names.
const (
	FieldChatID    = "chat_id"
	FieldMessageID = "message_id"
	FieldSessionID = "session_id"
	FieldDuration  = "duration_ms"
	FieldError     = "error"
	FieldOperation = "operation"

	// Common operational fields shared across packages.
	FieldPath        = "path"
	FieldReason      = "reason"
	FieldPanic       = "panic"
	FieldStack       = "stack"
	FieldGoroutine   = "goroutine"
	FieldDirectory   = "directory"
	FieldControlType = "control_type"

	// Claude-backend fields.
	FieldModel          = "model"
	FieldPermissionMode = "permission_mode"
	FieldEffortLevel    = "effort_level"
	FieldPromptLength   = "prompt_length"
	FieldEventType      = "event_type"
	FieldEventSubtype   = "event_subtype"
	FieldToolName       = "tool_name"
)

// LevelVar is a variable Level.
type LevelVar = slog.LevelVar

// New creates a new text logger with component tag.
func New(level *LevelVar, w io.Writer, component string) *Logger {
	return newLogger(slog.NewTextHandler(w, handlerOpts(level)), component)
}

// NewJSON creates a new JSON logger.
func NewJSON(level *LevelVar, w io.Writer, component string) *Logger {
	return newLogger(slog.NewJSONHandler(w, handlerOpts(level)), component)
}

// newLogger wraps a handler, attaching the component tag when set.
func newLogger(h slog.Handler, component string) *Logger {
	logger := slog.New(h)
	if component != "" {
		logger = logger.With("component", component)
	}
	return logger
}

// handlerOpts 共享 New / NewJSON 的 handler 选项：level + 统一时间戳格式。
func handlerOpts(level *LevelVar) *slog.HandlerOptions {
	return &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(a.Value.Time().Format("2006-01-02 15:04:05.000"))
			}
			return a
		},
	}
}

// FromString parses level from string.
func FromString(s string) (*LevelVar, error) {
	var lvl LevelVar
	switch s {
	case "debug":
		lvl.Set(LevelDebug)
	case "info":
		lvl.Set(LevelInfo)
	case "warn":
		lvl.Set(LevelWarn)
	case "error":
		lvl.Set(LevelError)
	default:
		return nil, fmt.Errorf("invalid log level: %s", s)
	}
	return &lvl, nil
}

// Nop returns a no-op logger.
func Nop() *Logger {
	return slog.New(&nopHandler{})
}

type nopHandler struct{}

func (n *nopHandler) Enabled(ctx context.Context, level slog.Level) bool { return false }
func (n *nopHandler) Handle(ctx context.Context, r slog.Record) error    { return nil }
func (n *nopHandler) WithAttrs(attrs []slog.Attr) slog.Handler           { return n }
func (n *nopHandler) WithGroup(name string) slog.Handler                 { return n }

// LogOperation records an operation with duration. Returns the error from fn.
// Usage: err := logOperation(logger, "operation_name", func() error { ... })
func LogOperation(l *Logger, operation string, fn func() error) error {
	start := time.Now()
	err := fn()
	duration := time.Since(start)

	// On failure emit only the Error line; logging "completed" at Info with
	// success=false first is redundant noise for operators tailing logs.
	if err != nil {
		l.With(FieldError, err.Error()).Error("operation failed",
			FieldOperation, operation,
			FieldDuration, duration.Milliseconds())
	} else {
		l.Info("operation completed",
			FieldOperation, operation,
			FieldDuration, duration.Milliseconds())
	}
	return err
}
