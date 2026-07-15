package config

import (
	"path/filepath"
	"time"
)

// applyDefaults fills zero-valued fields with sensible defaults.
// Called after JSON unmarshaling; env vars (expanded earlier) take
// precedence over these defaults.
func applyDefaults(cfg *Config, cfgPath string) {
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.LogOutput == "" {
		cfg.LogOutput = "stderr"
	}
	if cfg.LogFormat == "" {
		cfg.LogFormat = "text"
	}
	if cfg.FeishuDomain == "" {
		cfg.FeishuDomain = "feishu"
	}
	if cfg.FeishuLogLevel == "" {
		cfg.FeishuLogLevel = "info"
	}
	if cfg.IPCAddr == "" {
		cfg.IPCAddr = "localhost:6060"
	}
	if cfg.Claude.CLIPath == "" {
		cfg.Claude.CLIPath = "claude"
	}
	if cfg.Claude.PermissionMode == "" {
		cfg.Claude.PermissionMode = "acceptEdits"
	}
	if cfg.Claude.MaxConcurrent == 0 {
		cfg.Claude.MaxConcurrent = 4
	}
	if cfg.Claude.StreamHistory <= 0 {
		cfg.Claude.StreamHistory = 50
	}
	if cfg.Claude.AppendSystemPrompt == "" {
		cfg.Claude.AppendSystemPrompt = "你的回答应该简洁，通常不超过1000字"
	}
	// Interactive picker option lists. nil (the JSON zero value for a slice)
	// triggers the default. An explicit empty array [] would NOT match, but
	// JSON omitempty on the struct tag means an absent field unmarshals to
	// nil, which is the common case.
	if cfg.Claude.ModelOptions == nil {
		cfg.Claude.ModelOptions = []string{"haiku", "sonnet", "opus"}
	}
	if cfg.Claude.PermissionOptions == nil {
		// "default" is intentionally excluded: it hangs the non-interactive
		// -p subprocess. Values are the string forms of claude.PermissionMode*.
		cfg.Claude.PermissionOptions = []string{"acceptEdits", "plan", "bypassPermissions"}
	}
	if cfg.Claude.EffortOptions == nil {
		cfg.Claude.EffortOptions = []string{"low", "medium", "high", "xhigh", "max"}
	}
	if cfg.Claude.SettingsCacheTTL == 0 {
		cfg.Claude.SettingsCacheTTL = 3600
	}
	if cfg.Opencode.CLIPath == "" {
		cfg.Opencode.CLIPath = "opencode"
	}
	if cfg.Opencode.MaxConcurrent == 0 {
		cfg.Opencode.MaxConcurrent = 4
	}
	if cfg.Opencode.StreamHistory <= 0 {
		cfg.Opencode.StreamHistory = 50
	}
	if cfg.Opencode.ListCacheTTL == 0 {
		cfg.Opencode.ListCacheTTL = 3600
	}
	if cfg.StateDir == "" {
		// Default to the directory holding the config file so state
		// lands next to the config. Relative paths resolve to CWD.
		cfg.StateDir = filepath.Dir(cfgPath)
	}
	if cfg.RouterPath == "" {
		// Backend bindings (sessionID/directory/model/permission/etc.) persist
		// here; without it router.New runs in-memory and every redeploy resets
		// all bindings to defaults. Co-located with state_dir so both backends
		// share the conventional {state_dir}/router.v5.json path.
		cfg.RouterPath = filepath.Join(cfg.StateDir, "router.v5.json")
	}
	if cfg.Timeouts.BackendHealth == 0 {
		cfg.Timeouts.BackendHealth = Duration(90 * time.Second)
	}
}
