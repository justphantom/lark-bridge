//go:build linux || darwin

package opencode

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
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
	// Sanitised env: strip the bridge's own secrets so a user-run tool inside
	// the CLI cannot read them (Low#19). The CLI's own *_API_KEY survives.
	cmd.Env = cmdutil.SanitizeChildEnv()
	cmdutil.ApplyGroupCancel(cmd)
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
// can replace the cache entry in place under listMu. Concurrent cold misses
// are deduplicated via listInflight: only the leader forks the CLI; waiters
// block on its channel and then read the freshly-populated cache. This kills
// the CPU/memory amplification of a /model or /agent flood (刷屏) each spawning
// its own 25–50s `opencode models` / `agent list` fork. Errors are NOT cached:
// a waiter whose leader failed becomes the next leader and re-fetches.
func (c *Client) cachedList(
	ctx context.Context,
	cache **listCache,
	fetch func(context.Context) ([]string, error),
) ([]string, error) {
	if c.listTTL <= 0 {
		return fetch(ctx)
	}
	for {
		c.listMu.Lock()
		if c.listInflight == nil {
			c.listInflight = make(map[**listCache]chan struct{})
		}
		now := time.Now()
		if *cache != nil && now.Sub((*cache).fetchedAt) < c.listTTL {
			// Return a copy so a caller cannot mutate the cached slice (the
			// miss path stores a copy too; both paths must hand back an
			// independent slice for the contract to hold on every call).
			out := make([]string, len((*cache).values))
			copy(out, (*cache).values)
			c.listMu.Unlock()
			return out, nil
		}
		// A fetch already running for this slot? Wait for it, then re-loop
		// (the leader will have populated the cache on success).
		if ch, wait := c.listInflight[cache]; wait {
			c.listMu.Unlock()
			select {
			case <-ch:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		// Become the leader: register an inflight channel, release the lock,
		// and fork. Waiters landing while we hold the lock see the channel.
		ch := make(chan struct{})
		c.listInflight[cache] = ch
		c.listMu.Unlock()

		values, err := fetch(ctx)

		c.listMu.Lock()
		if err == nil {
			snapshot := make([]string, len(values))
			copy(snapshot, values)
			*cache = &listCache{values: snapshot, fetchedAt: time.Now()}
		}
		delete(c.listInflight, cache)
		close(ch)
		c.listMu.Unlock()
		// Leader returns its own result/error directly (waiters re-read the
		// cache via the loop, so both paths hand back independent slices).
		return values, err
	}
}
