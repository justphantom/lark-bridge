//go:build linux || darwin

package omp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// defaultListTimeout bounds `omp models --json` when Options.ListTimeout is
// not set. The subcommand fetches the provider catalog over the network
// before printing; measured at ~137s on omp/17.1.8, so 300s gives headroom.
const defaultListTimeout = 300 * time.Second

// defaultListCacheTTL bounds how long ListModels results stay cached when
// Options.ListCacheTTL is not set. omp cold-starts the listing at 100s+,
// so caching makes repeated /model pickers instant; 1h matches claude's
// settings cache horizon.
const defaultListCacheTTL = time.Hour

// modelsJSON mirrors the `omp models --json` payload (a single-line object).
// Only the selector field is consumed; the rest (name/provider/
// contextWindow/…) is ignored.
type modelsJSON struct {
	Models []struct {
		// Selector is the `provider/id` form omp's --model accepts, e.g.
		// "nvidia/z-ai/glm5" or "autoapi/agnes-2.0-flash".
		Selector string `json:"selector"`
	} `json:"models"`
}

// listCache holds a snapshot of ListModels output with the moment it was
// captured. A nil cache or one past listTTL is treated as a miss.
type listCache struct {
	values    []string
	fetchedAt time.Time
}

// ListModels runs `omp models --json` and returns one entry per model in the
// CLI's `provider/id` selector form. Results are cached for c.listTTL (default
// 1h); a cache miss forks the CLI (~100-150s). When listTTL <= 0 caching is
// disabled and every call forks.
//
// The fork is bounded by c.listTimeout (default 300s); the picker path wraps
// listFn in bridgebase.listFnTimeout (also 300s) as an outer cap, so the
// effective deadline is the shorter of the two (standard nested-ctx
// semantics) — an operator may set ListTimeout lower to fail fast.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	return c.cachedList(ctx, &c.modelsCache, c.execModels)
}

// execModels forks `omp models --json` and parses the single-line JSON payload
// into a selector slice. It does NOT acquire c.sem: the model list is a
// short-lived query relative to Run and should not queue behind minute-long
// prompt slots.
func (c *Client) execModels(ctx context.Context) ([]string, error) {
	if c.cliPath == "" {
		return nil, errors.New("omp: cli_path is empty")
	}
	ctx, cancel := context.WithTimeout(ctx, c.listTimeout)
	defer cancel()
	// #nosec G204 -- c.cliPath comes from the trusted config file; args are
	// a fixed subcommand, not user input.
	cmd := exec.CommandContext(ctx, c.cliPath, "models", "--json")
	// Sanitised env: strip the bridge's own secrets so a user-run tool inside
	// the CLI cannot read them (Low#19). The CLI's own *_API_KEY survives.
	cmd.Env = cmdutil.SanitizeChildEnv()
	cmdutil.ApplyGroupCancel(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("omp models --json: %w", err)
	}
	var parsed modelsJSON
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, fmt.Errorf("omp models --json: parse: %w", err)
	}
	values := make([]string, 0, len(parsed.Models))
	for _, m := range parsed.Models {
		if s := strings.TrimSpace(m.Selector); s != "" {
			values = append(values, s)
		}
	}
	if len(values) == 0 {
		return nil, errors.New("omp models --json: no models in output")
	}
	return values, nil
}

// cachedList serves a list query from cache when fresh, otherwise invokes
// fetch and stores its result. cache is a pointer-to-pointer so the miss path
// can replace the cache entry in place under listMu. Concurrent cold misses
// are deduplicated via listInflight: only the leader forks the CLI; waiters
// block on its channel and then read the freshly-populated cache. This kills
// the CPU/memory amplification of a /model flood (刷屏) each spawning its own
// 100–150s `omp models --json` fork. Errors are NOT cached: a waiter whose
// leader failed becomes the next leader and re-fetches (bounded by the number
// of waiters, and the picker's static-config fallback catches persistent
// failure).
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
