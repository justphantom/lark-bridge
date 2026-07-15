package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// validate performs semantic validation on a loaded config.
// Called after applyDefaults.
//
// Only cross-binary fields are validated here: log levels/output/format,
// duration floors, tool_summary ranges, state_dir writability, and per-binary
// fields that are present. Required-field checks that are specific to one
// binary (feishu creds for feishu-front, opencode connection for opencode-back)
// belong in that binary's main.go, because a config file for one binary
// legitimately omits another binary's fields.
func validate(cfg *Config) error {
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug/info/warn/error, got %q", cfg.LogLevel)
	}
	switch cfg.LogOutput {
	case "stderr", "stdout":
	default:
		return fmt.Errorf("log_output must be stderr or stdout, got %q", cfg.LogOutput)
	}
	switch cfg.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log_format must be text or json, got %q", cfg.LogFormat)
	}
	switch cfg.FeishuLogLevel {
	case "debug", "info", "warn", "error", "":
	default:
		return fmt.Errorf("feishu_log_level must be one of debug/info/warn/error, got %q", cfg.FeishuLogLevel)
	}
	// Validate component log levels.
	for comp, level := range map[string]string{
		"router":   cfg.ComponentLogLevels.Router,
		"opencode": cfg.ComponentLogLevels.Opencode,
		"feishu":   cfg.ComponentLogLevels.Feishu,
		"bridge":   cfg.ComponentLogLevels.Bridge,
		"dedup":    cfg.ComponentLogLevels.Dedup,
	} {
		if level == "" || level == "debug" || level == "info" || level == "warn" || level == "error" {
			continue
		}
		return fmt.Errorf("component_log_levels.%s must be one of debug/info/warn/error, got %q", comp, level)
	}

	// Claude fields. applyDefaults always populates cfg.Claude (cli_path,
	// permission_mode, max_concurrent, ...), so this always runs; harmless for
	// configs that omit claude since the defaults themselves validate.
	//
	// "default" is rejected: claude-back runs the CLI non-interactively
	// (-p --output-format stream-json), where an interactive permission
	// prompt would hang the subprocess forever.
	switch cfg.Claude.PermissionMode {
	case "acceptEdits", "plan", "bypassPermissions", "":
	default:
		return fmt.Errorf("claude.permission_mode must be one of acceptEdits/plan/bypassPermissions, got %q", cfg.Claude.PermissionMode)
	}
	if cfg.Claude.MaxConcurrent < 1 {
		return fmt.Errorf("claude.max_concurrent must be >= 1, got %d", cfg.Claude.MaxConcurrent)
	}

	// Opencode CLI fields. applyDefaults always populates cfg.Opencode. A value
	// < 1 is rejected; applyDefaults rewrites an unset (0) value to the default,
	// so 0 reaching here can only be an explicit negative number.
	if cfg.Opencode.MaxConcurrent < 1 {
		return fmt.Errorf("opencode.max_concurrent must be >= 1, got %d", cfg.Opencode.MaxConcurrent)
	}

	// StateDir writability.
	if cfg.StateDir != "" {
		stateDirAbs, err := filepath.Abs(cfg.StateDir)
		if err != nil {
			return fmt.Errorf("state_dir: failed to resolve absolute path: %w", err)
		}
		if err := ensureDir("state_dir", stateDirAbs, false); err != nil {
			return err
		}
	}

	// Timeout ranges.
	const minTunableTimeout = time.Second
	if d := time.Duration(cfg.Timeouts.BackendHealth); d > 0 && d < minTunableTimeout {
		return fmt.Errorf("timeouts.backend_health must be >= %s when set, got %s", minTunableTimeout, d)
	}
	if d := time.Duration(cfg.Timeouts.PromptTimeout); d > 0 && d < minTunableTimeout {
		return fmt.Errorf("timeouts.prompt_timeout must be >= %s when set, got %s", minTunableTimeout, d)
	}

	// Replay-guard ranges. Zero values are valid (means "use dispatcher default").
	if d := time.Duration(cfg.Dedup.StaleWindow); d > 0 && d < minTunableTimeout {
		return fmt.Errorf("dedup.stale_window must be >= %s when set, got %s", minTunableTimeout, d)
	}
	if d := time.Duration(cfg.Dedup.EventTTL); d > 0 && d < minTunableTimeout {
		return fmt.Errorf("dedup.event_ttl must be >= %s when set, got %s", minTunableTimeout, d)
	}
	if cfg.Dedup.EventMaxEntries < 0 {
		return fmt.Errorf("dedup.event_max_entries must be >= 0, got %d", cfg.Dedup.EventMaxEntries)
	}

	return nil
}

// ensureDir validates that abs is an existing directory, creating it
// recursively (0755) when create=true and it is missing. label prefixes
// errors. StateDir uses create=false (must pre-exist).
func ensureDir(label, abs string, create bool) error {
	info, err := os.Stat(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("%s: failed to access: %w", label, err)
		}
		if !create {
			return fmt.Errorf("%s: directory does not exist: %s", label, abs)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return fmt.Errorf("%s: failed to create directory: %w", label, err)
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: path is not a directory: %s", label, abs)
	}
	return nil
}
