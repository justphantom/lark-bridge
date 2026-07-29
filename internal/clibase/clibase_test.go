package clibase

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestCheckVersion_MissingBinary(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	err := CheckVersion(context.Background(), "/nonexistent-cli-xyz", "test", time.Second, logger)
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestCheckVersion_SuccessLogsVersion(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// `true --version` is supported on GNU coreutils and is a no-op binary
	// universally present on Linux/macOS dev boxes.
	err := CheckVersion(context.Background(), "true", "test", 5*time.Second, logger,
		"permission_mode", "acceptEdits")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	out := buf.String()
	if !contains(out, "test CLI ready") {
		t.Fatalf("missing ready log: %s", out)
	}
	if !contains(out, "permission_mode") {
		t.Fatalf("missing extra field: %s", out)
	}
	if !contains(out, "version") {
		t.Fatalf("missing version field: %s", out)
	}
}

func contains(s, substr string) bool {
	return bytes.Contains([]byte(s), []byte(substr))
}
