package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/config"
	"github.com/hu/lark-bridge/internal/log"
)

// writeSettingsFiles creates a fake settings directory with the given
// filenames and returns its absolute path. Used to test scanSettingsDir and
// ListSettings against a realistic file layout without touching ~/.claude.
func writeSettingsFiles(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("{}"), 0o600); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return dir
}

// TestScanSettingsDir_MatchesBothGlobs verifies settings.json and
// *-settings.json are both picked up, sorted by basename, and that unrelated
// files are excluded.
func TestScanSettingsDir_MatchesBothGlobs(t *testing.T) {
	dir := writeSettingsFiles(t,
		"settings.json",
		"kimi-settings.json",
		"zhipu-settings.json",
		"other.json",      // not a settings file
		"README.md",       // not json
		"mysettings.json", // no hyphen before "settings"
	)
	got, err := scanSettingsDir(dir)
	if err != nil {
		t.Fatalf("scanSettingsDir: %v", err)
	}
	// Expect the three settings files, sorted by basename. other.json,
	// README.md, mysettings.json must be excluded.
	want := []string{
		filepath.Join(dir, "kimi-settings.json"),
		filepath.Join(dir, "settings.json"),
		filepath.Join(dir, "zhipu-settings.json"),
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// TestScanSettingsDir_EmptyIsNotError verifies a directory with no matching
// files returns nil, nil (the picker surfaces "no settings files found").
func TestScanSettingsDir_EmptyIsNotError(t *testing.T) {
	dir := writeSettingsFiles(t, "other.json", "readme.txt")
	got, err := scanSettingsDir(dir)
	if err != nil {
		t.Fatalf("scanSettingsDir error: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for empty match, got %v", got)
	}
}

// TestScanSettingsDir_DeduplicatesByPath verifies that a file matching both
// globs (impossible in practice, but the dedup is cheap insurance) appears
// only once. settings.json does not match *-settings.json, so this is really
// a no-op regression guard.
func TestScanSettingsDir_NoDuplicates(t *testing.T) {
	dir := writeSettingsFiles(t, "settings.json", "a-settings.json")
	got, err := scanSettingsDir(dir)
	if err != nil {
		t.Fatalf("scanSettingsDir: %v", err)
	}
	seen := make(map[string]int)
	for _, p := range got {
		seen[p]++
	}
	for p, n := range seen {
		if n > 1 {
			t.Errorf("path %q appeared %d times", p, n)
		}
	}
}

// TestResolveSettingsDir verifies empty → ~/.claude, "~/..." → $HOME/...,
// relative → $HOME/rel, absolute → verbatim. The "~/.claude" case is the
// regression guard: without explicit tilde expansion filepath.Join would
// produce "/home/user/~/.claude" (literal tilde), which is never intended.
func TestResolveSettingsDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("HOME unset: %v", err)
	}
	cases := []struct {
		in, want string
	}{
		{"", filepath.Join(home, ".claude")},
		{"~", home},
		{"~/.claude", filepath.Join(home, ".claude")},
		{"~/custom-claude", filepath.Join(home, "custom-claude")},
		{".custom-claude", filepath.Join(home, ".custom-claude")},
		{"/etc/claude", "/etc/claude"},
	}
	for _, c := range cases {
		if got := resolveSettingsDir(c.in); got != c.want {
			t.Errorf("resolveSettingsDir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestListSettings_CacheHitsWithinTTL verifies a second call within the TTL
// does not rescan (same result, cache served).
func TestListSettings_CacheHitsWithinTTL(t *testing.T) {
	dir := writeSettingsFiles(t, "settings.json", "kimi-settings.json")
	c := New(config.Claude{SettingsDir: dir, SettingsCacheTTL: 3600}, log.Nop())

	first, err := c.ListSettings(context.Background())
	if err != nil {
		t.Fatalf("first ListSettings: %v", err)
	}
	// Add a file after the first scan; if caching works, the second call
	// must NOT see it.
	os.WriteFile(filepath.Join(dir, "new-settings.json"), []byte("{}"), 0o600)

	second, err := c.ListSettings(context.Background())
	if err != nil {
		t.Fatalf("second ListSettings: %v", err)
	}
	if !slices.Equal(first, second) {
		t.Errorf("cache should hit (no rescan):\n first=%v\n second=%v", first, second)
	}
}

// TestListSettings_CacheExpiresAfterTTL verifies that after the TTL the cache
// is refreshed (new files become visible).
func TestListSettings_CacheExpiresAfterTTL(t *testing.T) {
	dir := writeSettingsFiles(t, "settings.json")
	c := New(config.Claude{SettingsDir: dir, SettingsCacheTTL: 1}, log.Nop())

	first, err := c.ListSettings(context.Background())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first scan = %v, want 1 file", first)
	}

	os.WriteFile(filepath.Join(dir, "kimi-settings.json"), []byte("{}"), 0o600)
	time.Sleep(1100 * time.Millisecond)

	second, err := c.ListSettings(context.Background())
	if err != nil {
		t.Fatalf("post-TTL: %v", err)
	}
	if len(second) != 2 {
		t.Errorf("after TTL expiry want 2 files, got %v", second)
	}
}

// TestListSettings_CacheDisabledWhenTTLZeroOrNegative verifies <=0 turns
// caching off: every call rescans.
func TestListSettings_CacheDisabledWhenTTLZeroOrNegative(t *testing.T) {
	for _, ttl := range []int{0, -1} {
		ttl := ttl
		t.Run("", func(t *testing.T) {
			dir := writeSettingsFiles(t, "settings.json")
			c := New(config.Claude{SettingsDir: dir, SettingsCacheTTL: ttl}, log.Nop())
			if _, err := c.ListSettings(context.Background()); err != nil {
				t.Fatalf("first: %v", err)
			}
			os.WriteFile(filepath.Join(dir, "kimi-settings.json"), []byte("{}"), 0o600)
			got, err := c.ListSettings(context.Background())
			if err != nil {
				t.Fatalf("second: %v", err)
			}
			if len(got) != 2 {
				t.Errorf("caching off: want 2 files on rescan, got %v", got)
			}
		})
	}
}

// TestListSettings_EmptyDirIsNotError verifies a directory with no settings
// files returns nil, nil from the full ListSettings path (cache enabled).
func TestListSettings_EmptyDirIsNotError(t *testing.T) {
	dir := writeSettingsFiles(t, "other.json")
	c := New(config.Claude{SettingsDir: dir, SettingsCacheTTL: 3600}, log.Nop())
	got, err := c.ListSettings(context.Background())
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for no matches, got %v", got)
	}
}
