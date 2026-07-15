package opencode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// listTimeout bounds the model/agent listing subcommands. The opencode CLI
// has a heavy startup cost (provider/config load before the subcommand even
// runs): observed 25–50s wall-clock for `opencode models` / `agent list`.
// 90s gives headroom over the worst observed case while still bounding a
// genuinely hung process.
const listTimeout = 90 * time.Second

// hiddenAgents are opencode's internal agents (compaction/summary/title) that
// have no value as a user-selectable --agent. The CLI `agent list` output does
// not mark them hidden, so they are filtered by name here.
var hiddenAgents = map[string]struct{}{
	"compaction": {},
	"summary":    {},
	"title":      {},
}

// listCache holds a snapshot of a list subcommand's output with the moment it
// was captured. A nil cache or one past listTTL is treated as a miss.
type listCache struct {
	values    []string
	fetchedAt time.Time
}

// execLines runs `<cliPath> args...` and returns the non-empty trimmed lines
// of stdout. It mirrors IsReady's exec.CommandContext+Output pattern but is
// kept separate because list subcommands return data (not a health verdict)
// and warrant their own timeout. It does NOT acquire c.sem: list queries are
// short-lived relative to Run and should not queue behind minute-long slots.
func (c *Client) execLines(ctx context.Context, args ...string) ([]string, error) {
	if c.cliPath == "" {
		return nil, errors.New("opencode: cli_path is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	// #nosec G204 -- c.cliPath comes from the trusted config file; args are
	// fixed subcommands ("models" / "agent" "list"), not user input.
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("opencode %s: %w", strings.Join(args, " "), err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			lines = append(lines, s)
		}
	}
	return lines, nil
}

// ListModels runs `opencode models` and returns one entry per line in the
// CLI's `provider/model` form. Results are cached for c.listTTL (configured
// in seconds via ListCacheTTL); a cache miss forks the CLI (~25-50s). When
// listTTL <= 0 caching is disabled and every call forks.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	return c.cachedList(ctx, &c.modelsCache, func(ctx context.Context) ([]string, error) {
		return c.execLines(ctx, "models")
	})
}

// ListAgents runs `opencode agent list` and returns the names of user-visible
// agents. The CLI prints one `name (role)` line per agent followed by an
// indented permissions JSON block; only the `name (role)` lines are parsed
// (the bracketed permission lines lack " (" and are skipped). Hidden internal
// agents (compaction/summary/title) are filtered by name. Results are cached
// for c.listTTL like ListModels.
func (c *Client) ListAgents(ctx context.Context) ([]string, error) {
	return c.cachedList(ctx, &c.agentsCache, func(ctx context.Context) ([]string, error) {
		lines, err := c.execLines(ctx, "agent", "list")
		if err != nil {
			return nil, err
		}
		return parseAgents(lines), nil
	})
}

// parseAgents extracts user-visible agent names from `opencode agent list`
// lines. It is split out of ListAgents so the cache layer wraps the parsed
// result rather than the raw lines.
func parseAgents(lines []string) []string {
	var agents []string
	for _, l := range lines {
		// Only match agent header lines of the form "name (role)". Indented
		// permission blocks like `  [ {...} ]` are skipped implicitly.
		idx := strings.LastIndex(l, " (")
		if idx <= 0 {
			continue
		}
		name := strings.TrimSpace(l[:idx])
		if name == "" {
			continue
		}
		if _, hidden := hiddenAgents[name]; hidden {
			continue
		}
		agents = append(agents, name)
	}
	return agents
}

// cachedList serves a list query from cache when fresh, otherwise invokes
// fetch and stores its result. cache is a pointer-to-pointer so the miss path
// can replace the cache entry in place under listMu. Concurrent misses are
// NOT deduplicated: two goroutines hitting an expired cache both fork the
// CLI. The picker path is async and rare, so at most one extra fork is
// acceptable; dedup would add a singleflight/goroutine-per-key mechanism out
// of proportion to the benefit.
func (c *Client) cachedList(
	ctx context.Context,
	cache **listCache,
	fetch func(context.Context) ([]string, error),
) ([]string, error) {
	if c.listTTL <= 0 {
		return fetch(ctx)
	}
	now := time.Now()
	c.listMu.Lock()
	if *cache != nil && now.Sub((*cache).fetchedAt) < c.listTTL {
		out := (*cache).values
		c.listMu.Unlock()
		return out, nil
	}
	c.listMu.Unlock()

	values, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	// Copy so a caller cannot mutate the cached slice.
	snapshot := make([]string, len(values))
	copy(snapshot, values)
	c.listMu.Lock()
	*cache = &listCache{values: snapshot, fetchedAt: time.Now()}
	c.listMu.Unlock()
	return values, nil
}
