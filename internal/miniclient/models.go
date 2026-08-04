package miniclient

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// listModelsTimeout bounds the -list-models fork. The picker blocks the user's
// click; a tight cap surfaces a misconfigured endpoint / dead proxy quickly.
const listModelsTimeout = 15 * time.Second

// listModelsMaxBytes bounds the captured stdout so a hostile or buggy proxy
// cannot stream an unbounded body and OOM us. 4 MiB is far above any real
// /v1/models catalog; past it we fail rather than swallow the whole output.
const listModelsMaxBytes = 4 << 20

// ListModels runs `miniagent -list-models` and returns the endpoint's model ids
// (one id per stdout line). It replaces the bridge's former hand-rolled
// GET /v1/models: v2.0.0 re-added -list-models (fe85c16 had removed it), so the
// CLI owns the endpoint, auth, and retry once more — the bridge no longer
// carries its own LLM HTTP code.
//
// miniagent v3.1+ is config-only: -list-models resolves the provider from the
// miniagent.json at -config (须 defaults.model 或单一 provider，否则 CLI 报错).
// The bridge never passes -chat-url/-models-url (those flags are gone), nor
// -key-file (removed upstream post-3.4.0): the key is resolved via
// effectiveAPIKey (KeyFile path → read by the bridge) and injected as
// $MINIAGENT_API_KEY, same routing as Run.
func (c *Client) ListModels(ctx context.Context, configPath string) ([]string, error) {
	if c.cliPath == "" {
		return nil, fmt.Errorf("miniclient: cli_path is empty")
	}
	apiKey, err := c.effectiveAPIKey()
	if err != nil {
		return nil, err
	}
	// config-only：configPath 每轮覆盖（/config per-chat 切换）> client 启动默认。
	if configPath == "" {
		configPath = c.configPath
	}
	args := []string{"-list-models", "-config", configPath}

	ctx, cancel := context.WithTimeout(ctx, listModelsTimeout)
	defer cancel()
	// #nosec G204 -- c.cliPath comes from trusted config; args are built internally.
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	cmd.Env = append(cmdutil.SanitizeChildEnv(), "MINIAGENT_API_KEY="+apiKey)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// Bound stderr so a pathological run cannot exhaust memory; the head holds
	// the actionable diagnostic (missing key, bad endpoint, unknown flag on an
	// older binary — "flag provided but not defined: -list-models").
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start miniagent -list-models: %w (is %s built?)", err, c.cliPath)
	}
	// Bound the read: LimitReader caps at listModelsMaxBytes, then we discard
	// the rest so the subprocess cannot block on a full stdout pipe.
	raw, _ := io.ReadAll(io.LimitReader(stdout, listModelsMaxBytes+1))
	_, _ = io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("miniagent -list-models 失败：%w；stderr: %s；可用 /model <id> 手动指定", err, msg)
		}
		return nil, fmt.Errorf("miniagent -list-models 失败：%w；可用 /model <id> 手动指定", err)
	}
	if len(raw) > listModelsMaxBytes {
		return nil, fmt.Errorf("miniagent -list-models：输出超过 %d 字节上限", listModelsMaxBytes)
	}
	var ids []string
	for _, line := range strings.Split(string(raw), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
