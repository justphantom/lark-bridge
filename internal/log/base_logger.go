package log

import (
	"io"
	"os"
)

// BaseLogger carries the four pieces of state every CLI backend's per-
// component logger setup needs: the base logger (used for backendrpc.Run /
// metrics), the level variable (so component loggers can share the base
// when their override is invalid), the output writer (so all loggers
// share one sink), and the resolved format ("text" | "json").
type BaseLogger struct {
	Logger *Logger
	Level  *LevelVar
	Output io.Writer
	Format string // "text" (default) | "json"
}

// NewBaseLogger builds the base logger and the shared LevelVar from a config's
// four scalar log fields. Returns an error if the level string is invalid.
// Formerly inlined in opencode-back / omp-back main.go; lifted so the same
// shape feeds every CLI backend (claude-back's simple path can build its own
// without the LevelVar when it does not layer per-component overrides).
func NewBaseLogger(level, output, format, component string) (*BaseLogger, error) {
	lvl, err := FromString(level)
	if err != nil {
		return nil, err
	}
	var w io.Writer = os.Stderr
	if output == "stdout" {
		w = os.Stdout
	}
	var logger *Logger
	if format == "json" {
		logger = NewJSON(lvl, w, component)
	} else {
		logger = New(lvl, w, component)
	}
	return &BaseLogger{Logger: logger, Level: lvl, Output: w, Format: format}, nil
}

// ComponentLogger builds a component-tagged logger from the base, applying an
// optional per-component level override. If override is empty or invalid, the
// base level is used. The output sink and format are inherited from the base
// so every component shares one stream.
func (b *BaseLogger) ComponentLogger(component, override string) *Logger {
	level := override
	lvl, err := FromString(level)
	if err != nil || level == "" {
		lvl = b.Level
	}
	if b.Format == "json" {
		return NewJSON(lvl, b.Output, component)
	}
	return New(lvl, b.Output, component)
}
