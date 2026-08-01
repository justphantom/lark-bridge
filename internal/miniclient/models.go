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
// v3 changed the endpoint flags: bare mode requires -chat-url (full chat
// completions URL) and uses -models-url (full models URL) for the listing;
// config mode (-config <abspath>) reads endpoints from miniagent.json and
// must NOT pass -chat-url/-models-url.
//
// The API key follows the same routing as Run: $MINIAGENT_API_KEY env by
// default, or -key-file (a path, not the key) when KeyFile is configured — in
// which case the key is kept out of the subprocess env.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	if c.cliPath == "" {
		return nil, fmt.Errorf("miniclient: cli_path is empty")
	}
	args := []string{"-list-models"}
	if c.configPath != "" {
		// config 模式：-list-models 经 miniagent.json 解析 provider（须 defaults.model
		// 或单一 provider，否则 CLI 报错）。
		args = append(args, "-config", c.configPath)
	} else {
		// 裸模式：-chat-url 必需；-models-url 空则 ListAvailableModels 报错（/model <id> 仍可用）。
		if c.chatURL != "" {
			args = append(args, "-chat-url", c.chatURL)
		}
		if c.modelsURL != "" {
			args = append(args, "-models-url", c.modelsURL)
		}
	}
	if c.keyFile != "" {
		args = append(args, "-key-file", c.keyFile)
	}

	ctx, cancel := context.WithTimeout(ctx, listModelsTimeout)
	defer cancel()
	// #nosec G204 -- c.cliPath comes from trusted config; args are built internally.
	cmd := exec.CommandContext(ctx, c.cliPath, args...)
	if c.keyFile == "" {
		cmd.Env = append(cmdutil.SanitizeChildEnv(), "MINIAGENT_API_KEY="+c.apiKey)
	} else {
		cmd.Env = cmdutil.SanitizeChildEnv()
	}
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
