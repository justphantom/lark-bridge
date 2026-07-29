package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewBaseLogger_Defaults(t *testing.T) {
	b, err := NewBaseLogger("info", "stderr", "text", "test")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if b.Logger == nil || b.Level == nil || b.Output == nil {
		t.Fatalf("missing fields: %+v", b)
	}
	if b.Format != "text" {
		t.Errorf("Format = %q, want text", b.Format)
	}
}

func TestNewBaseLogger_InvalidLevel(t *testing.T) {
	if _, err := NewBaseLogger("bogus", "", "", ""); err == nil {
		t.Fatal("expected error on invalid level")
	}
}

func TestBaseLogger_ComponentLogger_Override(t *testing.T) {
	var buf bytes.Buffer
	// Build a base manually so we can capture output into our buffer.
	lvl, _ := FromString("info")
	base := &BaseLogger{Logger: New(lvl, &buf, "base"), Level: lvl, Output: &buf, Format: "text"}

	// Override to debug: the component logger should emit debug lines.
	cl := base.ComponentLogger("child", "debug")
	cl.Debug("hello")
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("debug override did not take effect: %s", buf.String())
	}
}

func TestBaseLogger_ComponentLogger_FallsBackOnInvalid(t *testing.T) {
	var buf bytes.Buffer
	lvl, _ := FromString("warn")
	base := &BaseLogger{Logger: New(lvl, &buf, "base"), Level: lvl, Output: &buf, Format: "text"}

	// Invalid override string → falls back to base level (warn), so info
	// messages are dropped.
	cl := base.ComponentLogger("child", "totally-not-a-level")
	cl.Info("dropped")
	if strings.Contains(buf.String(), "dropped") {
		t.Errorf("info surfaced under warn base: %s", buf.String())
	}
}
