package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// settingsGlobs are the filename patterns ListSettings matches inside the
// settings directory. settings.json is the Claude default; *-settings.json
// covers operator-named variants (kimi-settings.json, zhipu-settings.json).
// Two globs (not one *settings*.json) so mysettings.json without the hyphen
// is not swept in.
var settingsGlobs = []string{"settings.json", "*-settings.json"}

// settingsListCache holds a snapshot of the settings directory scan with the
// moment it was captured. A nil cache or one past settingsTTL is a miss.
type settingsListCache struct {
	paths     []string
	fetchedAt time.Time
}

// resolveSettingsDir turns the configured settings_dir into an absolute path.
// An empty dir resolves to ~/.claude (the Claude default). A leading "~" is
// expanded to $HOME so the conventional "~/.claude" works (Go's filepath.Join
// treats "~" as a literal char). A relative path is anchored at $HOME; an
// absolute path is used verbatim. os.UserHomeDir fails only if HOME is unset,
// in which case the input is returned as-is and ListSettings reports the
// misconfiguration.
func resolveSettingsDir(dir string) string {
	if filepath.IsAbs(dir) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return dir // best-effort: ListSettings will surface the empty dir
	}
	if dir == "" {
		return filepath.Join(home, ".claude")
	}
	// Expand a leading "~/" (or bare "~") to $HOME so config values like
	// "~/.claude" resolve correctly without depending on shell expansion.
	if dir == "~" {
		return home
	}
	if strings.HasPrefix(dir, "~/") {
		return filepath.Join(home, dir[2:])
	}
	return filepath.Join(home, dir)
}

// ListSettings returns the absolute paths of settings files in the settings
// directory, sorted by filename. Results are cached for c.settingsTTL; a cache
// miss rescans the directory. When settingsTTL <= 0 caching is disabled and
// every call rescans. An empty settingsDir (HOME unset and no config) yields
// an error so the picker can surface it instead of showing an empty card.
func (c *Client) ListSettings(_ context.Context) ([]string, error) {
	if c.settingsDir == "" {
		return nil, errNoSettingsDir
	}
	return c.cachedSettingsList()
}

// errNoSettingsDir is returned when neither config nor $HOME could resolve a
// settings directory. It is a sentinel so tests/callers can distinguish it
// from a scan of an empty directory (which is a valid nil-result, no error).
var errNoSettingsDir = errors.New("settings directory is not configured and $HOME is unset")

// cachedSettingsList serves ListSettings from cache when fresh, otherwise
// rescans. Concurrent misses are NOT deduplicated: the scan is sub-millisecond
// local I/O, so at most one redundant scan is acceptable; dedup would add a
// goroutine-per-key mechanism out of proportion to the benefit.
func (c *Client) cachedSettingsList() ([]string, error) {
	if c.settingsTTL <= 0 {
		return scanSettingsDir(c.settingsDir)
	}
	now := time.Now()
	c.settingsMu.Lock()
	if c.settingsCache != nil && now.Sub(c.settingsCache.fetchedAt) < c.settingsTTL {
		out := c.settingsCache.paths
		c.settingsMu.Unlock()
		return out, nil
	}
	c.settingsMu.Unlock()

	paths, err := scanSettingsDir(c.settingsDir)
	if err != nil {
		return nil, err
	}
	snapshot := make([]string, len(paths))
	copy(snapshot, paths)
	c.settingsMu.Lock()
	c.settingsCache = &settingsListCache{paths: snapshot, fetchedAt: time.Now()}
	c.settingsMu.Unlock()
	return paths, nil
}

// scanSettingsDir globs settingsGlobs under dir, deduplicates by basename
// (settings.json would also match *-settings.json only if named with a
// hyphen, which it is not, but the dedup is cheap insurance), and returns
// sorted absolute paths. A directory with no matching files returns nil,
// nil — the picker surfaces "no settings files found".
func scanSettingsDir(dir string) ([]string, error) {
	seen := make(map[string]struct{})
	var paths []string
	for _, g := range settingsGlobs {
		matches, err := filepath.Glob(filepath.Join(dir, g))
		if err != nil {
			// Glob only errors on malformed pattern; settingsGlobs are fixed
			// literals, so this is unreachable. Surface it anyway.
			return nil, err
		}
		for _, m := range matches {
			if _, dup := seen[m]; dup {
				continue
			}
			seen[m] = struct{}{}
			paths = append(paths, m)
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		return filepath.Base(paths[i]) < filepath.Base(paths[j])
	})
	return paths, nil
}
