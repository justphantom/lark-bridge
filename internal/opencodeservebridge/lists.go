package opencodeservebridge

import (
	"context"
	"time"
)

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

type listCache struct {
	values    []string
	fetchedAt time.Time
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
