package cliutil

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// versionBin is a binary whose `--version` exits 0; bash ships one on every
// CI/dev host. /bin/sh (dash) rejects --version, so it cannot be used here.
func versionBin(t *testing.T) string {
	t.Helper()
	for _, c := range []string{"bash", "/bin/bash", "/usr/bin/bash"} {
		if path, err := exec.LookPath(c); err == nil {
			return path
		}
	}
	t.Skip("no bash available for --version probe test")
	return ""
}

func testLogger(buf *bytes.Buffer) *log.Logger {
	var lvl log.LevelVar
	lvl.Set(log.LevelInfo)
	return log.New(&lvl, buf, "test")
}

// TestCheckVersion_Ready verifies a binary that prints a version succeeds,
// returns no error, and the ready line carries the version + extra fields.
func TestCheckVersion_Ready(t *testing.T) {
	bin := versionBin(t)
	var buf bytes.Buffer
	err := CheckVersion(context.Background(), bin, "sh", 5*time.Second, testLogger(&buf), "permission_mode", "bypass")
	if err != nil {
		t.Fatalf("CheckVersion: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"sh CLI ready", "cli_path", bin, "version"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q\ngot: %s", want, got)
		}
	}
	if !strings.Contains(got, "permission_mode") || !strings.Contains(got, "bypass") {
		t.Errorf("extraFields not logged\ngot: %s", got)
	}
}

// TestCheckVersion_MissingBinary verifies a non-existent path errors with
// the backend name and path in the message.
func TestCheckVersion_MissingBinary(t *testing.T) {
	err := CheckVersion(context.Background(), "/no/such/cli-test", "claude", 5*time.Second, testLogger(&bytes.Buffer{}))
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
	for _, want := range []string{"claude", "/no/such/cli-test"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

// TestCheckVersion_ContextCancelled verifies a pre-cancelled ctx errors
// fast instead of running the binary.
func TestCheckVersion_ContextCancelled(t *testing.T) {
	bin := versionBin(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckVersion(ctx, bin, "sh", 5*time.Second, testLogger(&bytes.Buffer{})); err == nil {
		t.Fatal("expected error on cancelled ctx, got nil")
	}
}

// TestCheckVersion_ExtraFieldsOptional verifies the zero-extraFields form
// still works (opencode passes none).
func TestCheckVersion_ExtraFieldsOptional(t *testing.T) {
	bin := versionBin(t)
	var buf bytes.Buffer
	if err := CheckVersion(context.Background(), bin, "sh", 5*time.Second, testLogger(&buf)); err != nil {
		t.Fatalf("CheckVersion: %v", err)
	}
	if !strings.Contains(buf.String(), "sh CLI ready") {
		t.Errorf("ready line missing\ngot: %s", buf.String())
	}
}
