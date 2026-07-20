package opencodeservebridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/log"
)

// readyTimeout bounds the startup Health probe. The serve server is already
// running when this client starts, so the probe is normally sub-second; 10s
// leaves room for a remote link without wedging startup.
const readyTimeout = 10 * time.Second

// listCacheTTLDefault is used when Config.ListCacheTTL is non-positive. The
// catalog rarely changes within a session, so a 10-minute TTL keeps the
// /model and /agent pickers instant on repeat invocations.
const listCacheTTLDefault = 10 * time.Minute

// hiddenAgents are opencode's internal agents (compaction/summary/title) that
// have no value as a user-selectable --agent. The /agent endpoint does not
// mark them hidden, so they are filtered by name as defence in depth.
var hiddenAgents = map[string]struct{}{
	"compaction": {},
	"summary":    {},
	"title":      {},
}

// AgentConfig carries the scalar settings the SDK-backed agent reads.
type AgentConfig struct {
	// BaseURL is the opencode serve root, e.g. "http://127.0.0.1:4096".
	BaseURL string
	// MaxConcurrent caps parallel in-flight Runs. The serve server already
	// serialises requests per session, so this only guards against runaway
	// per-chat fan-out. <=0 → default (4).
	MaxConcurrent int
}

// Agent wraps the opencode-go-sdk-lite Client + GlobalEventStream as the
// production opencodeAPI implementation. One global SSE connection lives for
// the lifetime of the process; each Run is one SDK Run (CreateSession or
// resume + Prompt + pump). Safe for concurrent use.
type Agent struct {
	baseURL string
	client  *oc.Client
	stream  *oc.GlobalEventStream
	logger  *log.Logger
	sem     chan struct{}

	listMu      sync.Mutex
	modelsCache *listCache
	agentsCache *listCache
}

type listCache struct {
	values    []string
	fetchedAt time.Time
}

// NewAgent builds an Agent. The SDK's GlobalEventStream goroutine is started
// here and lives until Close. The logger defaults to a no-op logger if nil.
func NewAgent(cfg AgentConfig, logger *log.Logger) (*Agent, error) {
	if logger == nil {
		logger = log.Nop()
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("opencodeserve: base_url is empty")
	}
	client, err := oc.New(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("opencodeserve: build sdk client: %w", err)
	}
	stream, err := client.NewGlobalEventStream(context.Background())
	if err != nil {
		return nil, fmt.Errorf("opencodeserve: open global stream: %w", err)
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = 4
	}
	return &Agent{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		client:  client,
		stream:  stream,
		logger:  logger,
		sem:     make(chan struct{}, n),
	}, nil
}

// SetLogger replaces the agent logger. main.go calls this after construction
// when wiring component loggers.
func (a *Agent) SetLogger(l *log.Logger) {
	if l != nil {
		a.logger = l
	}
}

// IsReady verifies the server is reachable and healthy via SDK Health.
func (a *Agent) IsReady(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()
	if err := a.client.Health(rctx); err != nil {
		return fmt.Errorf("opencode serve not ready: %w", err)
	}
	a.logger.Info("opencode serve ready", "base_url", a.baseURL)
	return nil
}

// Close stops the SDK GlobalEventStream. Idempotent.
func (a *Agent) Close() error {
	return a.stream.Close()
}

// Run starts one agent turn via SDK Run. The caller drains the returned
// HighEvent channel until close; a terminal event (result/error) precedes
// close. Blocks acquiring a concurrency slot until ctx is cancelled.
func (a *Agent) Run(ctx context.Context, opts oc.RunOptions) (<-chan oc.HighEvent, error) {
	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out, err := a.client.Run(ctx, a.stream, opts)
	if err != nil {
		<-a.sem
		return nil, err
	}
	// Wrap with a goroutine that releases the slot when the SDK closes the
	// channel. SDK Run does not expose its own pump completion to the caller,
	// so this drain-and-release is the only way to free the slot.
	released := make(chan oc.HighEvent, 16)
	go func() {
		defer func() { <-a.sem }()
		defer close(released)
		for ev := range out {
			select {
			case <-ctx.Done():
				// Drain remaining without forwarding; SDK closes on its own
				// ctx cancellation.
				return
			case released <- ev:
			}
		}
	}()
	return released, nil
}

// ListModels returns one "provider/model" entry per active model. Cached for
// listCacheTTLDefault.
func (a *Agent) ListModels(ctx context.Context) ([]string, error) {
	return a.cachedList(ctx, &a.modelsCache, func(ctx context.Context) ([]string, error) {
		models, err := a.client.ListModels(ctx, nil)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(models))
		for _, m := range models {
			if m.ID == "" || m.ProviderID == "" {
				continue
			}
			if m.Status == "deprecated" {
				continue
			}
			if !m.Enabled {
				continue
			}
			out = append(out, m.ProviderID+"/"+m.ID)
		}
		return out, nil
	})
}

// ListAgents returns user-visible agent ids. Cached for listCacheTTLDefault.
func (a *Agent) ListAgents(ctx context.Context) ([]string, error) {
	return a.cachedList(ctx, &a.agentsCache, func(ctx context.Context) ([]string, error) {
		agents, err := a.client.ListAgents(ctx, nil)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(agents))
		for _, ag := range agents {
			if ag.Hidden {
				continue
			}
			if _, hidden := hiddenAgents[ag.ID]; hidden {
				continue
			}
			if ag.ID == "" {
				continue
			}
			out = append(out, ag.ID)
		}
		return out, nil
	})
}

// AbortSession POSTs /api/session/{id}/interrupt via SDK. Idempotent.
func (a *Agent) AbortSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("abort: empty session id")
	}
	return a.client.Interrupt(ctx, sessionID)
}

// SwitchModel POSTs /api/session/{id}/model. spec is "provider/model" or
// empty (empty leaves the server default). Returns ok=false when spec is
// unparseable so the caller can surface a clear error.
func (a *Agent) SwitchModel(ctx context.Context, sessionID, spec string) error {
	ref, err := parseModelSpec(spec)
	if err != nil {
		return err
	}
	return a.client.SwitchModel(ctx, sessionID, ref)
}

// SwitchAgent POSTs /api/session/{id}/agent.
func (a *Agent) SwitchAgent(ctx context.Context, sessionID, agent string) error {
	if agent == "" {
		return errors.New("switch agent: empty name")
	}
	return a.client.SwitchAgent(ctx, sessionID, agent)
}

// parseModelSpec turns "provider/model" into an SDK ModelRef. Empty spec is
// allowed (clears the pin) and yields a zero ModelRef.
func parseModelSpec(spec string) (oc.ModelRef, error) {
	if spec == "" {
		return oc.ModelRef{}, nil
	}
	idx := strings.IndexByte(spec, '/')
	if idx <= 0 || idx == len(spec)-1 {
		return oc.ModelRef{}, fmt.Errorf("model spec must be provider/model: %q", spec)
	}
	return oc.ModelRef{ProviderID: spec[:idx], ID: spec[idx+1:]}, nil
}

// cachedList serves a list query from cache when fresh, otherwise invokes
// fetch and stores its result. Concurrent misses are NOT deduplicated: the
// picker path is rare and idempotent.
func (a *Agent) cachedList(
	ctx context.Context,
	cache **listCache,
	fetch func(context.Context) ([]string, error),
) ([]string, error) {
	now := time.Now()
	a.listMu.Lock()
	if *cache != nil && now.Sub((*cache).fetchedAt) < listCacheTTLDefault {
		out := (*cache).values
		a.listMu.Unlock()
		return out, nil
	}
	a.listMu.Unlock()

	values, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	// Do NOT cache an empty result: opencode serve loads its catalog
	// asynchronously after startup; caching the empty slice would pin
	// "没有可用的模型" for 10 minutes.
	if len(values) == 0 {
		return values, nil
	}
	snapshot := make([]string, len(values))
	copy(snapshot, values)
	a.listMu.Lock()
	*cache = &listCache{values: snapshot, fetchedAt: time.Now()}
	a.listMu.Unlock()
	return values, nil
}
