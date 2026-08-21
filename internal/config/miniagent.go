package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MiniAgentBackConfig is the miniagent-back binary's view of the union
// config document: the shared Core plus the sections only the backend owns.
// Its owned top-level keys are exactly what LoadMiniAgentBack decodes —
// feishu credentials and the frontend sections in the same file are skipped.
//
// The status_monitor section is owned here too (not just by the
// status-monitor binary): miniagent-back reuses status_monitor.interval as
// its own metrics-push period (backendrpc.StartMetricsLoop), which is the
// established behaviour from the union-Config era.
type MiniAgentBackConfig struct {
	Core

	MiniAgent     MiniAgent     `json:"miniagent,omitempty"`
	StatusMonitor StatusMonitor `json:"status_monitor,omitempty"`
}

// MiniAgent configures the miniagent backend (cmd/miniagent-back). The bridge
// forks the miniagent CLI per turn; these fields map to CLI flags. miniagent
// v3.1+ removed bare CLI mode entirely (-chat-url/-models-url/-context-window/
// -shell-timeout are gone) and requires config mode: the endpoints and the
// removed run settings live in the miniagent.json referenced by ConfigPath,
// generated at deploy time by deploy.sh. The bridge therefore passes
// -config <ConfigPath> and only the per-turn flags below; endpoint/shell-timeout
// /context-window must be edited in miniagent-cli.json, not here.
//
// miniagent v4.2.0 removed -system/-max-tokens, so the system prompt and the
// max output-token cap are no longer bridge-config fields: they now live in
// miniagent-cli.json (defaults.system_prompt / run.max_tokens), set on the
// miniagent side. The struct below carries only the surviving per-turn flags.
type MiniAgent struct {
	// APIKey authenticates to the OpenAI-compatible endpoint. Use ${VAR} to
	// pull from the environment; reaches the subprocess as $MINIAGENT_API_KEY
	// (the upstream key chain is provider.key → $MINIAGENT_API_KEY; -key-file
	// was removed post-3.4.0 — KeyFile, if set, is read by the bridge and
	// injected via this same env var, taking precedence over APIKey).
	APIKey string `json:"api_key,omitempty"`
	// Model is the model id passed as -model each turn (config mode: a bare id
	// also accepted by the upstream CLI). Required: ${MINIAGENT_DEFAULT_MODEL}.
	Model string `json:"model,omitempty"`
	// Provider is the provider name passed as -provider each turn, PAIRED with
	// Model. miniagent post-v4.0.1 (02f8f81) split -model (bare id) from
	// -provider and requires them together: a Model without Provider leaves the
	// pair inert (buildArgs omits both and miniagent.json's defaults apply).
	// Optional: ${MINIAGENT_DEFAULT_PROVIDER}.
	Provider string `json:"provider,omitempty"`
	// StreamHistory caps per-run raw NDJSON captures under
	// {stateDir}/streams/miniagent/. 0 → 50; negative → disable.
	StreamHistory int `json:"stream_history,omitempty"`
	// WorkspaceRoot is the REQUIRED global workdir. Bounds the /cd picker and
	// is the default -workdir (miniagent requires an absolute -workdir).
	// [P1: required, enforced in cmd/miniagent-back/main.go]
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	// Stream enables -stream (SSE reasoning_delta → live 思考区). Default false.
	Stream bool `json:"stream,omitempty"`
	// MaxIterations caps one turn's LLM-call count (-max-iterations). <=0 → 20.
	MaxIterations int `json:"max_iterations,omitempty"`
	// Thinking is the reasoning effort (-thinking): off|minimal|low|medium|high|
	// xhigh|max. Default "off" (applyDefaults). [P2]
	Thinking string `json:"thinking,omitempty"`
	// KeyFile reads the API key from a file. miniagent removed -key-file
	// (post-3.4.0), so the bridge reads the file itself and injects the value
	// via $MINIAGENT_API_KEY on the subprocess — KeyFile now only keeps the key
	// out of lark-bridge's own config/env, not out of the miniagent subprocess
	// env. Key isolation relies on OS permissions (dedicated user + 0600),
	// matching miniagent's README. Takes precedence over APIKey when set.
	KeyFile string `json:"key_file,omitempty"`
	// ConfigDir is the directory scanned for the miniagent config file
	// (miniagent.json or *-miniagent.json). Empty/unset → ~/.miniagent at
	// runtime via os.UserHomeDir, so the config layer stays independent of
	// the process user's HOME. When set, the bridge resolves the concrete
	// ConfigPath before forking the CLI; leaving it unset lets the CLI use
	// its own default (~/.miniagent/miniagent.json).
	ConfigDir string `json:"config_dir,omitempty"`
	// ConfigPath is the path passed as -config to the miniagent CLI (optional:
	// empty means the CLI falls back to its own default ~/.miniagent/miniagent.json).
	// When set, it may be a bare name (e.g. "kimi-miniagent.json") — the bridge
	// resolves it relative to ConfigDir if it is not absolute. deploy.sh still
	// generates /etc/lark-bridge/miniagent-cli.json for the default deployment
	// path. The resolved absolute path is stored in ConfigPath after applyDefaults.
	ConfigPath string `json:"config_path,omitempty"`
}

// applyMiniAgentDefaults fills the backend-owned section's defaults.
func applyMiniAgentDefaults(cfg *MiniAgentBackConfig) {
	// StreamHistory: negative disables archiving.
	if cfg.MiniAgent.StreamHistory == 0 {
		cfg.MiniAgent.StreamHistory = 50
	}
	// Thinking default for v3 (-thinking). Empty falls under the valid-enum
	// switch in validate, but filling it here means a turn always sends an
	// explicit flag value and /thinking can show a real default. (-mode was
	// removed in miniagent v5.0.0 and has no bridge config counterpart.)
	if cfg.MiniAgent.Thinking == "" {
		cfg.MiniAgent.Thinking = "off"
	}
}

// validateMiniAgent checks the backend-owned section.
func validateMiniAgent(cfg *MiniAgentBackConfig) error {
	// MiniAgent enum fields. applyDefaults always populates Thinking, so
	// reaching validate with "" means an explicit clear; the default value
	// itself is valid. (-mode was removed in miniagent v5.0.0.)
	// (WorkspaceRoot/ConfigPath requireds are binary-specific
	// → enforced in cmd/miniagent-back/main.go.)
	switch cfg.MiniAgent.Thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max", "":
	default:
		return fmt.Errorf("miniagent.thinking must be one of off/minimal/low/medium/high/xhigh/max, got %q", cfg.MiniAgent.Thinking)
	}
	return nil
}

// ResolveConfigDir returns the absolute directory scanned for the miniagent
// config file, defaulting to ~/.miniagent when ConfigDir is unset. It mirrors
// the resolution inside ResolveConfigPath so the /config picker lists exactly
// the directory the startup scan read. Returns "" only when ConfigDir is unset
// AND the home directory cannot be determined.
func ResolveConfigDir(cfg *MiniAgentBackConfig) string {
	if cfg.MiniAgent.ConfigDir != "" {
		if filepath.IsAbs(cfg.MiniAgent.ConfigDir) {
			return cfg.MiniAgent.ConfigDir
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return cfg.MiniAgent.ConfigDir
		}
		if cfg.MiniAgent.ConfigDir == "~" {
			return home
		}
		if strings.HasPrefix(cfg.MiniAgent.ConfigDir, "~/") {
			return filepath.Join(home, cfg.MiniAgent.ConfigDir[2:])
		}
		return filepath.Join(home, cfg.MiniAgent.ConfigDir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".miniagent")
}

// ResolveConfigPath resolves the miniagent config path for the bridge.
// Logic (mirrors claude-back's resolveSettingsDir + ListSettings):
//   - If ConfigPath is explicitly set:
//   - Absolute path → used as-is.
//   - Relative or "~" path → anchored at ConfigDir (default ~/.miniagent).
//   - If ConfigPath is empty:
//   - Scan ConfigDir (default ~/.miniagent) for miniagent.json first,
//     then *-miniagent.json (operator-named variants). The first match wins.
//   - If nothing found, return "" so the CLI uses its own default.
//
// The returned path is always absolute (or empty when no default was found).
// When empty, the bridge omits -config from the CLI args and lets miniagent
// fall back to its own ~/.miniagent/miniagent.json default.
func ResolveConfigPath(cfg *MiniAgentBackConfig) string {
	cfgDir := ResolveConfigDir(cfg)
	cp := cfg.MiniAgent.ConfigPath

	// Explicit ConfigPath: resolve to absolute.
	if cp != "" {
		if filepath.IsAbs(cp) {
			return cp
		}
		// Expand "~" prefix (Go's filepath.Join treats "~" as literal).
		if cp == "~" {
			home, _ := os.UserHomeDir()
			return home
		}
		if strings.HasPrefix(cp, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return cp
			}
			return filepath.Join(home, cp[2:])
		}
		// Relative name → anchored at ConfigDir.
		if cfgDir != "" {
			return filepath.Join(cfgDir, cp)
		}
		return cp
	}

	// Empty ConfigPath: scan ConfigDir for the default config.
	if cfgDir == "" {
		return ""
	}
	// First try the canonical name.
	p := filepath.Join(cfgDir, "miniagent.json")
	if _, err := os.Stat(p); err == nil {
		abs, _ := filepath.Abs(p)
		return abs
	} else if !errors.Is(err, os.ErrNotExist) {
		// Stat error (e.g. permission denied) — surface it rather than
		// silently falling through, so the operator can diagnose.
		return ""
	}
	// Then scan for operator-named variants (*-miniagent.json).
	matches, err := filepath.Glob(filepath.Join(cfgDir, "*-miniagent.json"))
	if err != nil {
		return ""
	}
	if len(matches) == 0 {
		return ""
	}
	// Pick the first match (sorted by Glob's lexicographic order).
	abs, _ := filepath.Abs(matches[0])
	return abs
}
