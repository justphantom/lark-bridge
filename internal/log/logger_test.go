package log

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	var buf bytes.Buffer
	var lvl LevelVar
	lvl.Set(LevelInfo)
	logger := New(&lvl, &buf, "test")
	logger.Info("test message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Errorf("expected log message, got %q", output)
	}
	if !strings.Contains(output, "component=test") {
		t.Errorf("expected component tag, got %q", output)
	}
}

func TestNewJSON(t *testing.T) {
	var buf bytes.Buffer
	var lvl LevelVar
	lvl.Set(LevelInfo)
	logger := NewJSON(&lvl, &buf, "test")
	logger.Info("test message", "key", "value")

	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Errorf("expected log message, got %q", output)
	}
	if !strings.Contains(output, "\"component\":\"test\"") {
		t.Errorf("expected component in JSON, got %q", output)
	}
}

func TestNop(t *testing.T) {
	logger := Nop()
	logger.Info("should not appear") // should not panic
	logger.Debug("debug message")
	logger.Warn("warn message")
	logger.Error("error message")
}

func TestFromString(t *testing.T) {
	tests := []struct {
		input   string
		wantErr bool
	}{
		{"debug", false},
		{"info", false},
		{"warn", false},
		{"warning", true},
		{"error", false},
		{"invalid", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			_, err := FromString(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("FromString(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	var lvl LevelVar
	lvl.Set(LevelWarn)
	logger := New(&lvl, &buf, "test")

	// Debug and Info should not appear
	logger.Debug("debug message")
	logger.Info("info message")
	output := buf.String()
	if strings.Contains(output, "debug message") || strings.Contains(output, "info message") {
		t.Errorf("expected debug/info messages to be filtered, got %q", output)
	}

	// Warn and Error should appear
	logger.Warn("warn message")
	logger.Error("error message")
	output = buf.String()
	if !strings.Contains(output, "warn message") {
		t.Errorf("expected warn message, got %q", output)
	}
	if !strings.Contains(output, "error message") {
		t.Errorf("expected error message, got %q", output)
	}
}

func TestLogOperation(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&LevelVar{}, &buf, "test")

	// Success path.
	if err := LogOperation(logger, "op-ok", func() error { return nil }); err != nil {
		t.Fatalf("LogOperation success returned %v", err)
	}
	out := buf.String()
	for _, want := range []string{"operation completed", "op-ok", "duration_ms"} {
		if !strings.Contains(out, want) {
			t.Errorf("success log missing %q: %s", want, out)
		}
	}

	// Failure path: error is returned and a failure line is logged.
	buf.Reset()
	got := LogOperation(logger, "op-fail", func() error { return errors.New("boom") })
	if got == nil || got.Error() != "boom" {
		t.Fatalf("LogOperation failure returned %v", got)
	}
	out = buf.String()
	if !strings.Contains(out, "operation failed") || !strings.Contains(out, "boom") {
		t.Errorf("failure log missing fields: %s", out)
	}
	// Failure must emit only the Error line, not a redundant Info "completed".
	if strings.Contains(out, "operation completed") {
		t.Errorf("failure log should not contain info line: %s", out)
	}
}
