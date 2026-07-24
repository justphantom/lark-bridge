package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// modelsRequestTimeout bounds the GET /v1/models call. The picker blocks
// the user's click, but the model list itself is usually a fast auth-gated
// index; a tight cap surfaces a misconfigured proxy quickly.
const modelsRequestTimeout = 15 * time.Second

// modelsResponse mirrors the OpenAI-compatible /v1/models payload. Only
// the id field is consumed; the rest is ignored.
type modelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// fetchModels lists model ids from an OpenAI-compatible endpoint
// ({baseURL}/v1/models). It exists because miniagent's own CLI no longer
// exposes -list-models after the stateless refactor (fe85c16): the bridge
// fetches the list itself via the standard library, so no third-party HTTP
// client is pulled in.
//
// Returns an error on any non-2xx / parse failure; the caller surfaces it
// to the user with a hint to fall back to /model <id>. baseURL must be the
// provider root without a /v1/... suffix (matching miniagent's own
// endpoint() convention).
func fetchModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	base, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("miniagent: base_url %q 无效", baseURL)
	}
	u := base.JoinPath("/v1/models")

	reqCtx, cancel := context.WithTimeout(ctx, modelsRequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("miniagent: 构建 models 请求失败：%w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("miniagent: 请求 /v1/models 失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("miniagent: 读取 models 响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 404 typically means the provider/proxy does not implement /v1/models
		// (one-api/new-api gate it behind config). Tell the user to hand-type.
		return nil, fmt.Errorf("miniagent: /v1/models 返回 %d，端点可能未实现；可用 /model <id> 手动指定", resp.StatusCode)
	}
	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("miniagent: 解析 models 响应失败：%w", err)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}
