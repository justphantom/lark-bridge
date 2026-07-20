package opencodeserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// readyTimeout bounds the GET /config readiness probe. The serve server is
// already running when this client starts, so the probe is normally
// sub-second; 10s leaves room for a remote link without wedging startup.
const readyTimeout = 10 * time.Second

// listCacheTTLDefault is used when Config.ListCacheTTL is non-positive. The
// catalog rarely changes within a session, so a 10-minute TTL keeps the
// /model and /agent pickers instant on repeat invocations without forking
// the server.
const listCacheTTLDefault = 10 * time.Minute

// hiddenAgents are opencode's internal agents (compaction/summary/title) that
// have no value as a user-selectable --agent. The /agent endpoint does not
// mark them hidden, so they are filtered by name here.
var hiddenAgents = map[string]struct{}{
	"compaction": {},
	"summary":    {},
	"title":      {},
}

// Config carries the scalar settings the serve client reads. Mirrors
// opencode.Config for parity so the bridge's HandlerConfig can stay
// shape-compatible across modes.
type Config struct {
	// BaseURL is the opencode serve root, e.g. "http://127.0.0.1:4096".
	// Required.
	BaseURL string
	// MaxConcurrent caps parallel in-flight sessions. The serve server
	// already serialises requests per session, so this only guards against
	// runaway per-chat fan-out. <=0 → default (4).
	MaxConcurrent int
	// ListCacheTTL bounds how long /model and /agent results stay cached.
	// <=0 → 10 minutes.
	ListCacheTTL int
}

// Client wraps a running `opencode serve` HTTP server. It owns one global
// /event SSE subscription that fans out per-session events to each in-flight
// Run; each Run is one POST /session/{id}/message?async=true. Safe for
// concurrent use.
type Client struct {
	baseURL    string
	httpClient *http.Client
	logger     *log.Logger
	dispatcher *sseDispatcher
	sem        chan struct{}

	listTTL     time.Duration
	listMu      sync.Mutex
	modelsCache *listCache
	agentsCache *listCache
}

// listCache holds a snapshot of a listing query with the moment it was
// captured. A nil cache or one past listTTL is treated as a miss.
type listCache struct {
	values    []string
	fetchedAt time.Time
}

// New builds a Client. The SSE dispatcher goroutine is started here and
// lives until Close. The logger defaults to a no-op logger if nil.
func New(cfg Config, logger *log.Logger) (*Client, error) {
	if logger == nil {
		logger = log.Nop()
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("opencodeserve: base_url is empty")
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("opencodeserve: invalid base_url %q", cfg.BaseURL)
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = 4
	}
	listTTL := time.Duration(cfg.ListCacheTTL) * time.Second
	if listTTL <= 0 {
		listTTL = listCacheTTLDefault
	}
	c := &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		httpClient: &http.Client{Timeout: 0}, // per-request deadlines via ctx
		logger:     logger,
		sem:        make(chan struct{}, n),
		listTTL:    listTTL,
	}
	c.dispatcher = newSSEDispatcher(c.baseURL, c.httpClient, c.logger)
	go c.dispatcher.run()
	return c, nil
}

// SetLogger replaces the client logger (and the dispatcher's). main.go calls
// this after construction when wiring component loggers.
func (c *Client) SetLogger(l *log.Logger) {
	if l == nil {
		return
	}
	c.logger = l
	c.dispatcher.setLogger(l)
}

// IsReady verifies the server is reachable and answering JSON by GET-ing
// /config. Returns an error suitable for a startup health gate.
func (c *Client) IsReady(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	_, err := c.fetchJSON(ctx, "/config", nil)
	if err != nil {
		return fmt.Errorf("opencode serve not ready (%s): %w", c.baseURL, err)
	}
	c.logger.Info("opencode serve ready", "base_url", c.baseURL)
	return nil
}

// ListModels returns one "provider/model" entry per active model the server
// knows about. Cached for c.listTTL. The serve /api/model endpoint returns
// the full catalog from every connected provider; deprecated and disabled
// entries are filtered.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	return c.cachedList(ctx, &c.modelsCache, func(ctx context.Context) ([]string, error) {
		raw, err := c.fetchJSON(ctx, "/api/model", nil)
		if err != nil {
			return nil, err
		}
		return parseModels(raw)
	})
}

// ListAgents returns user-visible agent ids. Hidden internal agents
// (compaction/summary/title, or hidden=true in the payload) are filtered.
// Cached for c.listTTL.
func (c *Client) ListAgents(ctx context.Context) ([]string, error) {
	return c.cachedList(ctx, &c.agentsCache, func(ctx context.Context) ([]string, error) {
		raw, err := c.fetchJSON(ctx, "/api/agent", nil)
		if err != nil {
			return nil, err
		}
		return parseAgents(raw)
	})
}

// Close stops the SSE dispatcher goroutine. Idempotent.
func (c *Client) Close() error {
	c.dispatcher.stop()
	return nil
}

// fetchJSON GETs path with optional headers and returns the raw JSON body.
// The Accept: application/json header is set by default so the SPA fallback
// does not return HTML for the JSON-only endpoints.
func (c *Client) fetchJSON(ctx context.Context, path string, headers map[string]string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.httpClient.Do(req) //nolint:gosec,bodyclose // G704: baseURL trusted; body closed via drainAndClose
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		drainAndClose(resp.Body)
		return nil, fmt.Errorf("%s: %w", path, apiError(resp.StatusCode, truncateDetail(body)))
	}
	// Bound the success body too: a malicious or stuck upstream could
	// stream a multi-GB JSON response and OOM the process. The largest
	// legitimate payload here is the model/agent catalog (~tens of KB).
	data, err := io.ReadAll(io.LimitReader(resp.Body, drainLimit))
	drainAndClose(resp.Body)
	return data, err
}

// cachedList serves a list query from cache when fresh, otherwise invokes
// fetch and stores its result. cache is a pointer-to-pointer so the miss
// path can replace the cache entry in place under listMu. Concurrent misses
// are NOT deduplicated: the picker path is rare and idempotent.
func (c *Client) cachedList(
	ctx context.Context,
	cache **listCache,
	fetch func(context.Context) ([]string, error),
) ([]string, error) {
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
	// Do NOT cache an empty result: opencode serve loads its catalog
	// asynchronously after startup (plugin providers populate over tens of
	// seconds), and the first picker call can race that window — fetching an
	// empty data array with HTTP 200. Caching that empty slice for listTTL
	// would pin "没有可用的模型" for 10 minutes. A retry on the next call
	// re-fetches and picks up the now-populated catalog.
	if len(values) == 0 {
		return values, nil
	}
	snapshot := make([]string, len(values))
	copy(snapshot, values)
	c.listMu.Lock()
	*cache = &listCache{values: snapshot, fetchedAt: time.Now()}
	c.listMu.Unlock()
	return values, nil
}

// catalogEnvelope is the {location, data} shape returned by /api/model and
// /api/agent. The location metadata is not read by the bridge.
type catalogEnvelope[T any] struct {
	Location json.RawMessage `json:"location"`
	Data     []T             `json:"data"`
}

// catalogModel is one entry in /api/model's data array. Only the fields the
// list picker reads are modelled; capabilities/cost/limit/etc. are ignored.
type catalogModel struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Status     string `json:"status"`  // "active"|"deprecated"|...
	Enabled    *bool  `json:"enabled"` // pointer so the default (absent) is distinct from false
}

func parseModels(raw json.RawMessage) ([]string, error) {
	var env catalogEnvelope[catalogModel]
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse /api/model: %w", err)
	}
	out := make([]string, 0, len(env.Data))
	for _, m := range env.Data {
		if m.ID == "" || m.ProviderID == "" {
			continue
		}
		if m.Status == "deprecated" {
			continue
		}
		if m.Enabled != nil && !*m.Enabled {
			continue
		}
		out = append(out, m.ProviderID+"/"+m.ID)
	}
	return out, nil
}

// catalogAgent is one entry in /api/agent's data array. The serve schema uses
// `id` as the agent identifier (the value passed to --agent); `name` is a
// human label that may be absent.
type catalogAgent struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Hidden bool   `json:"hidden"`
}

func parseAgents(raw json.RawMessage) ([]string, error) {
	var env catalogEnvelope[catalogAgent]
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse /api/agent: %w", err)
	}
	out := make([]string, 0, len(env.Data))
	for _, a := range env.Data {
		if a.Hidden {
			continue
		}
		// The server's own hidden internal agents (compaction/summary/title)
		// are already flagged hidden=true in the payload; keep the local
		// filter as defence in depth against future schema drift.
		if _, hidden := hiddenAgents[a.ID]; hidden {
			continue
		}
		if a.ID == "" {
			continue
		}
		out = append(out, a.ID)
	}
	return out, nil
}
