package router

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/log"
)

// TestMigrateLegacyBindings_CopiesWhenTargetMissing verifies the one-time R2
// migration: a pre-split deployment's bindings live in the shared
// router.v5.json; on the first run of a backend with its new per-backend
// filename, MigrateLegacyBindings must carry those bindings forward so the
// first save does not reset them to empty.
func TestMigrateLegacyBindings_CopiesWhenTargetMissing(t *testing.T) {
	dir := t.TempDir()
	// Seed the legacy shared file with real bindings.
	legacy := filepath.Join(dir, legacyRouterFile)
	writeRaw(t, legacy, `{"version":5,"bindings":{"chatA":{"sessionID":"sess-A","directory":"/a"}}}`)

	target := filepath.Join(dir, "router-claude.v5.json")
	MigrateLegacyBindings(target, log.Nop())

	r, err := New(target, log.Nop())
	if err != nil {
		t.Fatalf("New migrated target: %v", err)
	}
	defer r.Close()
	got, ok := r.Lookup("chatA")
	if !ok {
		t.Fatal("expected chatA binding carried over from legacy file")
	}
	if got.SessionID != "sess-A" || got.Directory != "/a" {
		t.Fatalf("migrated binding fields wrong: %+v", got)
	}
	// Legacy file left in place for operator rollback.
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("legacy file should remain for rollback: %v", err)
	}
}

// TestMigrateLegacyBindings_NoopWhenTargetExists verifies a second run (target
// already populated) does not overwrite the per-backend file — the per-backend
// file is authoritative once created.
func TestMigrateLegacyBindings_NoopWhenTargetExists(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, legacyRouterFile)
	writeRaw(t, legacy, `{"version":5,"bindings":{"legacy":{"session_id":"OLD"}}}`)

	target := filepath.Join(dir, "router-claude.v5.json")
	writeRaw(t, target, `{"version":5,"bindings":{"fresh":{"session_id":"NEW"}}}`)

	MigrateLegacyBindings(target, log.Nop())

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !strings.Contains(string(data), "NEW") {
		t.Fatalf("existing target should be untouched; got %s", data)
	}
	if strings.Contains(string(data), "OLD") {
		t.Fatalf("legacy binding should NOT have overwritten existing target; got %s", data)
	}
}

// TestMigrateLegacyBindings_NoopWhenLegacyAbsent verifies a clean install
// (no legacy file) produces no migration, no file, no error.
func TestMigrateLegacyBindings_NoopWhenLegacyAbsent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "router-claude.v5.json")

	MigrateLegacyBindings(target, log.Nop())

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target should not exist after no-op migration; err=%v", err)
	}
}

// TestMigrateLegacyBindings_EmptyTargetPath is a safety no-op.
func TestMigrateLegacyBindings_EmptyTargetPath(t *testing.T) {
	// Must not panic.
	MigrateLegacyBindings("", log.Nop())
}
