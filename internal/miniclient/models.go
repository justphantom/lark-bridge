package miniclient

import (
	"bytes"
	"context"
	"encoding/json"
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

// ModelRef is one available provider/model pair. It mirrors miniagent's
// -list-models NDJSON model event ({"type":"model","provider","model"}).
//
// provider and model are SEPARATE fields because a model id may itself contain
// '/' (OpenRouter-style "org/model"), so a concatenated "provider/model"
// string is ambiguous to split back. miniagent v4 (commits 02f8f81 + 2099241,
// post-v4.0.1 / unreleased on the miniagent main branch) split the CLI into
// paired -provider/-model flags and changed -list-models to emit one NDJSON
// model event per line (aggregating ALL providers, each line carrying its own
// provider). The bridge keeps the pair together end-to-end so it can pass them
// back as a matched -provider/-model pair on Run.
type ModelRef struct {
	Provider string
	Model    string
}

// ListModels runs `miniagent -list-models` and returns the endpoint's available
// models as provider/model pairs (one ModelRef per stdout NDJSON model event).
// It replaces the bridge's former hand-rolled GET /v1/models: v2.0.0 re-added
// -list-models (fe85c16 had removed it), so the CLI owns the endpoint, auth,
// and retry once more — the bridge no longer carries its own LLM HTTP code.
//
// miniagent v3.1+ is config-only: -list-models resolves the provider(s) from
// the miniagent.json at -config. The bridge never passes -chat-url/-models-url
// (those flags are gone), nor -key-file (removed upstream post-3.4.0): the key
// is resolved via effectiveAPIKey (KeyFile path → read by the bridge) and
// injected as $MINIAGENT_API_KEY, same routing as Run.
//
// Output contract (miniagent post-v4.0.1, 2099241): stdout is NDJSON, one JSON
// object per line shaped {"type":"model","provider":"<name>","model":"<id>"}.
// Non-JSON lines (stderr bleed-through under partial failure, where stdout and
// stderr are merged by the capturing harness) and lines whose type != "model"
// are skipped. NOTE: this is incompatible with the v4.0.1 tag's former
// plain-text "provider/model_id" lines — the bridge now requires miniagent HEAD
// (or the upcoming tagged release carrying 2099241).
func (c *Client) ListModels(ctx context.Context, configPath string) ([]ModelRef, error) {
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
	// NDJSON: one model event per line. miniagent (2099241) exits 1 on partial
	// failure but still emits the successful entries on stdout before doing so;
	// that path is handled above (cmd.Wait err). Here every parse failure or
	// non-model line is skipped rather than fatal, so a stray stderr line
	// merged into stdout cannot blank the whole catalog.
	var refs []ModelRef
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "model" || ev.Model == "" {
			continue
		}
		refs = append(refs, ModelRef{Provider: ev.Provider, Model: ev.Model})
	}
	return refs, nil
}
