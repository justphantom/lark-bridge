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
	// StreamHistory: 0 (unset) → default 50; negative → explicit disable so
	// streamarchive.NewSink's history<=0 disable branch becomes reachable
	// (matches ListCacheTTL/SettingsCacheTTL's "negative = off" semantics).
	if cfg.Claude.StreamHistory == 0 {
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
	// StreamHistory: see Claude note — negative disables archiving.
	if cfg.Opencode.StreamHistory == 0 {
		cfg.Opencode.StreamHistory = 50
	}
	if cfg.Opencode.ListCacheTTL == 0 {
		cfg.Opencode.ListCacheTTL = 3600
	}
	// OMP CLI defaults. ApprovalMode defaults to "write" (≈ claude
	// acceptEdits), NOT "yolo" (≈ bypassPermissions): the latter auto-executes
	// dangerous tool calls without confirmation, which conflicts with the
	// project's default safety posture (§12 risk table). Operators who want
	// yolo must set it explicitly in config. ThinkingLevel defaults to "auto"
	// so the CLI picks the level per model.
	if cfg.OMP.CLIPath == "" {
		cfg.OMP.CLIPath = "omp"
	}
	if cfg.OMP.MaxConcurrent == 0 {
		cfg.OMP.MaxConcurrent = 4
	}
	// StreamHistory: see Claude note — negative disables archiving.
	if cfg.OMP.StreamHistory == 0 {
		cfg.OMP.StreamHistory = 50
	}
	if cfg.OMP.AppendSystemPrompt == "" {
		cfg.OMP.AppendSystemPrompt = "你的回答应该简洁，通常不超过1000字"
	}
	if cfg.OMP.ApprovalMode == "" {
		cfg.OMP.ApprovalMode = "write"
	}
	if cfg.OMP.ThinkingLevel == "" {
		cfg.OMP.ThinkingLevel = "auto"
	}
	// ModelOptions defaults to nil: model availability is deployment-dependent
	// (§A.1 observed claude-haiku 403, §A.3 observed glm-5.2 upstream 120s
	// timeout), so no list is compiled in. config.example.json offers a
	// suggested list; operators adjust per actually-available models.
	if cfg.OMP.ApprovalOptions == nil {
		cfg.OMP.ApprovalOptions = []string{"always-ask", "write", "yolo"}
	}
	if cfg.OMP.ThinkingOptions == nil {
		cfg.OMP.ThinkingOptions = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max", "auto"}
	}
	if cfg.OMP.MaxAutoRetries == 0 {
		cfg.OMP.MaxAutoRetries = 3
	}
	if cfg.OMP.GCColdArchiveAfterDays <= 0 {
		cfg.OMP.GCColdArchiveAfterDays = 30
	}
	if cfg.OMP.GCRetainNewestPerCwd <= 0 {
		cfg.OMP.GCRetainNewestPerCwd = 5
	}
	if cfg.OMP.GCTimeout == 0 {
		cfg.OMP.GCTimeout = Duration(300 * time.Second)
	}
	if cfg.DeployMonitor.DeployTarget == "" {
		cfg.DeployMonitor.DeployTarget = "deploy"
	}
	if cfg.StatusMonitor.Interval == 0 {
		cfg.StatusMonitor.Interval = Duration(60 * time.Second)
	}
	if cfg.MiniAgent.SystemPrompt == "" {
		cfg.MiniAgent.SystemPrompt = "你是一个简洁的工程助手，回答通常不超过 500 字。重点先行；不确定先查别猜；副作用操作前说明；遇错说原因和修复，别盲目重试。"
	}
	if cfg.MiniAgent.MaxTokens <= 0 {
		cfg.MiniAgent.MaxTokens = 4096
	}
	// StreamHistory: see Claude note — negative disables archiving.
	if cfg.MiniAgent.StreamHistory == 0 {
		cfg.MiniAgent.StreamHistory = 50
	}
	// Mode/Thinking defaults for v3 (-mode/-thinking replaced -confine). Empty
	// falls under the valid-enum switches in validate, but filling them here
	// means a turn always sends an explicit flag value (matching the configured
	// safety posture) and /mode /thinking can show a real default.
	if cfg.MiniAgent.Mode == "" {
		cfg.MiniAgent.Mode = "default"
	}
	if cfg.MiniAgent.Thinking == "" {
		cfg.MiniAgent.Thinking = "off"
	}
	if cfg.StateDir == "" {
		// Default to the directory holding the config file so state
		// lands next to the config. Relative paths resolve to CWD.
		cfg.StateDir = filepath.Dir(cfgPath)
	}
	if cfg.RouterPath == "" {
		// Backend bindings (sessionID/directory/model/permission/etc.)
		// persist here; without it router.New runs in-memory and every
		// redeploy resets all bindings to defaults. This is the LEGACY
		// shared path — since the per-backend router split (R2) each backend
		// overrides it with router-<backend>.v5.json under state_dir, and
		// this value only serves as the backward-compat fallback (and as
		// the source for one-time legacy migration). Co-located with
		// state_dir so the conventional {state_dir}/router.v5.json path
		// holds.
		cfg.RouterPath = filepath.Join(cfg.StateDir, "router.v5.json")
	}
	if cfg.Timeouts.BackendHealth == 0 {
		cfg.Timeouts.BackendHealth = Duration(90 * time.Second)
	}
	if cfg.Timeouts.UsageSessionTTL == 0 {
		cfg.Timeouts.UsageSessionTTL = Duration(7 * 24 * time.Hour)
	}
	if cfg.Timeouts.CardPatchDelay == 0 {
		cfg.Timeouts.CardPatchDelay = Duration(5 * time.Second)
	}
	// StreamArchiveRedact defaults to true (P1): NDJSON archives contain
	// prompts, file contents, and tool output that may include secrets. The
	// field is *bool, so nil (omitted) → default ON and explicit false → OFF.
	// RedactStreams() resolves the final value.
	if cfg.StreamArchiveRedact == nil {
		t := true
		cfg.StreamArchiveRedact = &t
	}
	// FileConvert: only apply defaults when the operator has opted in
	// (Enabled). An absent / disabled section keeps the legacy "reject file
	// messages" behaviour; we do not synthesise inbox paths the operator
	// never asked for.
	if cfg.FileConvert.Enabled {
		if cfg.FileConvert.MaxFileSize <= 0 {
			cfg.FileConvert.MaxFileSize = 30 << 20 // 30 MiB
		}
		if cfg.FileConvert.ConvertTimeout == 0 {
			cfg.FileConvert.ConvertTimeout = Duration(60 * time.Second)
		}
		if cfg.FileConvert.Retention == 0 {
			cfg.FileConvert.Retention = Duration(7 * 24 * time.Hour)
		}
		// xlsx formula mode defaults to cached value (decision 6A). An
		// operator who leaves it empty gets "value"; an explicit empty is
		// indistinguishable from absent under JSON, so treat both as default.
		if cfg.FileConvert.XlsxFormulaMode == "" {
			cfg.FileConvert.XlsxFormulaMode = "value"
		}
	}
}

// RedactStreams reports whether stream-archive redaction is enabled, resolving
// the tri-state StreamArchiveRedact: nil (operator left it unset) defaults to
// true; an explicit *false disables redaction. applyDefaults normalizes
// nil → &true at load time, so the nil branch is a defensive fallback for any
// caller that runs before defaults are applied.
func (c *Config) RedactStreams() bool {
	return c.StreamArchiveRedact == nil || *c.StreamArchiveRedact
}
