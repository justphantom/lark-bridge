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

// newSDKClient builds the SDK client, attaching HTTP Basic auth when either
// Username or Password is set (opencode serve only checks the password; the
// username defaults to "opencode" server-side).
func newSDKClient(cfg AgentConfig) (*oc.Client, error) {
	if cfg.Username == "" && cfg.Password == "" {
		return oc.New(cfg.BaseURL)
	}
	return oc.New(cfg.BaseURL, oc.WithBasicAuth(cfg.Username, cfg.Password))
}

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
	// Username/Password are HTTP Basic auth credentials; both empty means
	// no Authorization header.
	Username string
	Password string
	// MaxConcurrent caps parallel in-flight Runs. The serve server already
	// serialises requests per session, so this only guards against runaway
	// per-chat fan-out. <=0 → default (4).
	MaxConcurrent int
}

// Agent wraps the opencode-go-sdk-lite Client as the production opencodeAPI
// implementation. The v1 event bus is isolated by directory, so SSE streams
// are pooled per working directory (lazy, lives until Close); each Run is one
// SDK Run (CreateSession or resume + Prompt + pump) on its directory's
// stream. Safe for concurrent use.
type Agent struct {
	baseURL string
	client  *oc.Client
	logger  *log.Logger
	sem     chan struct{}

	streamsMu sync.Mutex
	streams   map[string]*oc.GlobalEventStream

	listMu      sync.Mutex
	modelsCache *listCache
	agentsCache *listCache
}

type listCache struct {
	values    []string
	fetchedAt time.Time
}

// NewAgent builds an Agent. Event streams are created lazily per directory on
// the first Run. The logger defaults to a no-op logger if nil.
func NewAgent(cfg AgentConfig, logger *log.Logger) (*Agent, error) {
	if logger == nil {
		logger = log.Nop()
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("opencodeserve: base_url is empty")
	}
	client, err := newSDKClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("opencodeserve: build sdk client: %w", err)
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = 4
	}
	return &Agent{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		client:  client,
		logger:  logger,
		sem:     make(chan struct{}, n),
		streams: make(map[string]*oc.GlobalEventStream),
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

// ReplyPermission forwards a permission answer to the serve server.
func (a *Agent) ReplyPermission(ctx context.Context, requestID, directory, reply, message string) error {
	return a.client.ReplyPermission(ctx, requestID, directory, reply, message)
}

// ReplyQuestion forwards question answers to the serve server.
func (a *Agent) ReplyQuestion(ctx context.Context, requestID, directory string, r *oc.QuestionReply) error {
	return a.client.ReplyQuestion(ctx, requestID, directory, r)
}

// RejectQuestion declines a pending question request.
func (a *Agent) RejectQuestion(ctx context.Context, requestID, directory string) error {
	return a.client.RejectQuestion(ctx, requestID, directory)
}

// Close stops all pooled SDK GlobalEventStreams. Idempotent.
func (a *Agent) Close() error {
	a.streamsMu.Lock()
	streams := a.streams
	a.streams = make(map[string]*oc.GlobalEventStream)
	a.streamsMu.Unlock()
	var err error
	for _, s := range streams {
		if e := s.Close(); e != nil {
			err = e
		}
	}
	return err
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
	out, err := a.client.Run(ctx, a.streamFor(opts.Location), opts)
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

// ListModels returns one "provider/model" entry per active model belonging to
// a connected provider. Cached for listCacheTTLDefault.
//
// SDK v1 的 ListModels 拍平 GET /provider 全部 provider 的模型目录（5518+
// 条），全量塞进飞书卡片会触发 ErrCode 11310 element exceeds the limit。
// 用 ListConnectedProviders 拿 serve 实际配置了凭证的 provider id 子集
// （实测通常 1~5 个），按它过滤后才落到卡片可承载的量级。
func (a *Agent) ListModels(ctx context.Context) ([]string, error) {
	return a.cachedList(ctx, &a.modelsCache, func(ctx context.Context) ([]string, error) {
		// Connected 是全局配置，拉取与模型目录分开调用；失败时退化为
		// 不过滤（保留旧行为），避免 serve 短暂抖动让卡片整个不返回。
		connected, connErr := a.client.ListConnectedProviders(ctx)
		connectedSet := make(map[string]struct{}, len(connected))
		if connErr == nil {
			for _, id := range connected {
				connectedSet[id] = struct{}{}
			}
		}
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
			// Connected 拉取成功时按它过滤；失败时（len=0）跳过过滤，
			// 由 bridgebase.maxQuestionOptions 截断兜底。
			if len(connectedSet) > 0 {
				if _, ok := connectedSet[m.ProviderID]; !ok {
					continue
				}
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
			if _, hidden := hiddenAgents[ag.Name]; hidden {
				continue
			}
			if ag.Name == "" {
				continue
			}
			out = append(out, ag.Name)
		}
		return out, nil
	})
}

// AbortSession POSTs /session/{id}/abort via SDK. Idempotent.
func (a *Agent) AbortSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("abort: empty session id")
	}
	return a.client.Interrupt(ctx, sessionID)
}

// ListSessions returns the sessions of one project directory; an empty
// directory falls back to the serve process's own CWD project.
func (a *Agent) ListSessions(ctx context.Context, directory string) ([]oc.SessionInfo, error) {
	if directory == "" {
		return a.client.ListSessions(ctx, nil)
	}
	return a.client.ListSessions(ctx, &oc.ListSessionsOpt{Directory: directory})
}

// SessionStatuses returns the status map of all sessions.
func (a *Agent) SessionStatuses(ctx context.Context) (map[string]oc.SessionStatus, error) {
	return a.client.SessionStatuses(ctx)
}

// DeleteSessionIfIdle deletes a session only if it is idle.
func (a *Agent) DeleteSessionIfIdle(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("delete idle: empty session id")
	}
	return a.client.DeleteSessionIfIdle(ctx, sessionID)
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
