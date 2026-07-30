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
// can replace the cache entry in place under listMu. Concurrent misses are
// NOT deduplicated (see opencode/client_list.go for the rationale: the picker
// path is async and rare, so at most one extra fork is acceptable).
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
