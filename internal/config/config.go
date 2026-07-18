// Package config loads and validates bridge configuration from a JSON file.
//
// The merged Config is the union of the claude and opencode source configs
// plus the new IPC fields (BackendID/FrontendURL/RouterPath) added by the
// 1-frontend/N-backend split. Each of the three binaries reads only
// the subset of fields it owns; cross-binary required-field checks are the
// responsibility of each binary's main.go, not this shared Load — a config
// file for a backend does not need Feishu credentials, and vice versa.
//
// Pipeline: readRaw -> expandEnvVars -> json.Unmarshal -> applyDefaults -> validate.
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/justphantom/lark-bridge/internal/strutil"
)

// envVarPattern is the shared ${VAR} matcher defined once in strutil so the
// config loader and strutil.ExpandEnvVars cannot drift apart on the surface
// syntax.
var envVarPattern = strutil.EnvVarPattern

// Config is the bridge top-level configuration. Fields are tagged with their
// owning binary so a config file can carry the union without confusion.
type Config struct {
	// —— 飞书凭证：feishu-front 用；后端忽略 ——
	FeishuAppID     string `json:"feishu_app_id"`
	FeishuAppSecret string `json:"feishu_app_secret"`
	FeishuDomain    string `json:"feishu_domain,omitempty"`
	FeishuLogLevel  string `json:"feishu_log_level,omitempty"`

	// —— 进程间通信：前端校验、后端携带，二者共享 ——
	BackendID   string `json:"backend_id,omitempty"`   // 在前端 registry 的唯一标识
	FrontendURL string `json:"frontend_url,omitempty"` // 前端 IPC server 地址
	IPCAddr     string `json:"ipc_addr,omitempty"`     // 前端 IPC 监听地址（仅 feishu-front 用）
	IPCSecret   string `json:"ipc_secret,omitempty"`   // 前端与后端共享密钥，校验 SSE/POST 的 Authorization: Bearer
	RouterPath  string `json:"router_path,omitempty"`  // router 持久化文件路径（前后端共用）

	// —— 后端运行时：各后端按需 ——
	Claude        Claude        `json:"claude,omitempty"`         // claude-back 用
	Opencode      Opencode      `json:"opencode,omitempty"`       // opencode-back 用
	DeployMonitor DeployMonitor `json:"deploy_monitor,omitempty"` // deploy-monitor 用
	MiniAgent     MiniAgent     `json:"miniagent,omitempty"`      // miniagent-back 用

	// —— 日志：共用 ——
	LogLevel           string            `json:"log_level"`
	LogOutput          string            `json:"log_output,omitempty"`
	LogFormat          string            `json:"log_format,omitempty"`
	ComponentLogLevels ComponentLogLevel `json:"component_log_levels,omitempty"` // opencode 源有
	LogDebugRedact     bool              `json:"log_debug_redact,omitempty"`     // opencode 源有

	StateDir string   `json:"state_dir,omitempty"`
	Timeouts Timeouts `json:"timeouts,omitempty"` // 两源并集

	// —— 防重放：feishu-front 用，后端忽略 ——
	Dedup DedupConfig `json:"dedup,omitempty"`
}

// Claude holds settings for the local Claude Code CLI subprocess that
// acts as the agent backend. The claude-back binary shells out to the
// `claude` CLI per turn and reads a stream-json event flow from stdout.
type Claude struct {
	CLIPath            string `json:"cli_path,omitempty"`             // path to the claude binary (default "claude")
	PermissionMode     string `json:"permission_mode,omitempty"`      // acceptEdits | plan | bypassPermissions ("default" hangs the non-interactive -p stream)
	DefaultDirectory   string `json:"default_directory,omitempty"`    // base dir for per-chat session working dirs
	MaxConcurrent      int    `json:"max_concurrent,omitempty"`       // max parallel CLI invocations (default 4)
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"` // system prompt to append (default: "你的回答应该简洁，通常不超过1000字")
	// StreamHistory caps how many recent per-run raw stream-json captures
	// are kept under {state_dir}/streams. <=0/unset → 50. The archive is
	// best-effort and stores lines verbatim (no redaction); see claudebridge.
	StreamHistory int `json:"stream_history,omitempty"`

	// ModelOptions lists the models offered in the interactive /model picker
	// card. nil/unset → ["haiku","sonnet","opus"]. Values are passed verbatim
	// to the CLI as --model; the picker also offers a custom-input box so a
	// model not listed can still be typed.
	ModelOptions []string `json:"model_options,omitempty"`
	// PermissionOptions lists the modes offered in the interactive /perm
	// picker card. nil/unset → [acceptEdits, plan, bypassPermissions]. The
	// picker has no custom-input box: "default" is intentionally excluded by
	// default as it hangs the non-interactive -p subprocess, but an operator
	// who understands the risk may add it here.
	PermissionOptions []string `json:"permission_options,omitempty"`
	// EffortOptions lists the levels offered in the interactive /effort
	// picker card. nil/unset → [low, medium, high, xhigh, max]. No
	// custom-input box; the picker restricts selection to listed values.
	EffortOptions []string `json:"effort_options,omitempty"`

	// SettingsDir is the directory scanned for the interactive /settings
	// picker (settings.json and *-settings.json). Empty/unset → the Client
	// resolves to ~/.claude at runtime via os.UserHomeDir, so the config
	// layer stays independent of the process user's HOME.
	SettingsDir string `json:"settings_dir,omitempty"`
	// SettingsCacheTTL bounds how long ListSettings results stay cached
	// (seconds). The scan is cheap (local fs), but caching keeps repeated
	// /settings pickers instant and mirrors opencode's list_cache_ttl.
	// 0/unset → 3600; negative disables caching.
	SettingsCacheTTL int `json:"settings_cache_ttl,omitempty"`
}

// Opencode holds settings for the local opencode CLI subprocess that acts
// as the agent backend. The opencode-back binary shells out to the `opencode`
// CLI per turn and reads an NDJSON event flow from stdout.
//
// The legacy HTTP-mode fields (base_url/username/password) are retained for
// backward compatibility with existing config files but are no longer used by
// opencode-back in CLI mode; they are ignored.
type Opencode struct {
	// CLI mode (current):
	CLIPath          string `json:"cli_path,omitempty"`          // path to the opencode binary (default "opencode")
	DefaultDirectory string `json:"default_directory,omitempty"` // base dir for per-chat session working dirs
	MaxConcurrent    int    `json:"max_concurrent,omitempty"`    // max parallel CLI invocations (default 4)

	// StreamHistory caps how many recent per-run raw NDJSON captures
	// are kept under {state_dir}/streams. <=0/unset -> 50. The archive is
	// best-effort and stores lines verbatim (no redaction); see opencodebridge.
	StreamHistory int `json:"stream_history,omitempty"`

	// ListCacheTTL bounds how long ListModels/ListAgents results stay cached
	// (seconds). The opencode CLI takes 25-50s to list, so caching makes
	// repeated /model and /agent pickers instant. <=0/unset -> 3600 (1h).
	// Set to a negative value to disable caching entirely.
	ListCacheTTL int `json:"list_cache_ttl,omitempty"`
}

// DeployMonitor holds settings for the lark-deploy-monitor backend, which
// receives /deploy prompts and runs `make <DeployTarget>` in ProjectRoot.
type DeployMonitor struct {
	// ProjectRoot is the repo root where `make` runs. Empty → working dir
	// of the monitor process (set in config; systemd sets WorkingDirectory).
	ProjectRoot string `json:"project_root,omitempty"`
	// DeployTarget is the make target invoked (default "deploy").
	DeployTarget string `json:"deploy_target,omitempty"`
}

// MiniAgent holds settings for the miniagent backend, a self-contained
// ReAct agent (LLM + tools + memory) that does NOT shell out to an external
// agent CLI like claude/opencode. It calls an OpenAI-compatible chat
// completions endpoint directly via net/http.
type MiniAgent struct {
	// APIKey authenticates to the OpenAI-compatible endpoint. Use ${VAR} to
	// pull from the environment (config.Load expands it); writing the key
	// literally in the config file is discouraged.
	APIKey string `json:"api_key,omitempty"`
	// BaseURL is the OpenAI-compatible root (no /v1/... suffix), e.g.
	// "https://api.openai.com" or a compatible provider's root like
	// "https://api.deepseek.com". Required: use ${MINIAGENT_BASE_URL} in
	// the config (config.Load rejects an unset/empty ${VAR}).
	BaseURL string `json:"base_url,omitempty"`
	// Model is the model id passed as the request "model" field (e.g.
	// "gpt-4o", "deepseek-chat"). Required: use ${MINIAGENT_DEFAULT_MODEL}
	// in the config (config.Load rejects an unset/empty ${VAR}).
	Model string `json:"model,omitempty"`
	// SystemPrompt is prepended to every turn as the system message. Empty
	// → a concise default assistant persona in config_defaults.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// MaxTokens caps one completion's output tokens. <=0/unset → 4096.
	MaxTokens int `json:"max_tokens,omitempty"`
	// WorkspaceRoot bounds read_file to paths under this directory (after
	// filepath.Clean). Empty → read_file is not registered (the LLM cannot
	// call it). Recommended: ${WORKSPACE_ROOT} so it shares the same env
	// var as claude-back / opencode-back.
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	// MemoryEnabled toggles per-chat conversation history (jsonl under
	// {state_dir}/miniagent/history/). nil/unset → enabled. Set false to
	// run stateless (each prompt independent, like P1).
	MemoryEnabled *bool `json:"memory_enabled,omitempty"`
	// Permission controls the tool execution policy (aligned with claude's
	// permission_mode):
	//   "plan"   — read-only (read_file + webfetch only; no write/shell)
	//   "default"— full tools with workspace bounds + shell blocklist
	//   "free"   — full tools without path/cwd/blocklist limits
	// Env key isolation and output truncation are ALWAYS enforced.
	// Empty → "default". Backward compat: if unset, falls back to
	// SecurityLevel ("default"/"free" map directly).
	Permission    string `json:"permission,omitempty"`
	SecurityLevel string `json:"security_level,omitempty"` // deprecated, use Permission
	// ShellBlockedPatterns overrides the default shell blocklist. When empty,
	// the built-in defaults (rm -rf, mkfs, dd if=, shutdown, etc.) are used.
	// Set to an empty array [] to disable all blocking (not recommended).
	ShellBlockedPatterns []string `json:"shell_blocked_patterns,omitempty"`
}

// ComponentLogLevel configures per-component log level overrides.
type ComponentLogLevel struct {
	Router        string `json:"router,omitempty"`
	Opencode      string `json:"opencode,omitempty"`
	Feishu        string `json:"feishu,omitempty"`
	Bridge        string `json:"bridge,omitempty"`
	Dedup         string `json:"dedup,omitempty"`
	DeployMonitor string `json:"deploy_monitor,omitempty"`
}

// Duration is a time.Duration that JSON-encodes as a Go duration
// string ("5m", "60s") rather than nanoseconds. It is a named type
// because Go does not allow methods on time.Duration itself.
type Duration time.Duration

// UnmarshalJSON parses a Go duration string. A field absent from the
// JSON stays at its zero value (0) and is filled by applyDefaults; an
// explicitly-supplied non-positive value ("0", "-5s") is rejected
// here so it cannot be silently overwritten by applyDefaults.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: expect a string like %q: %w", "5m", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration: parse %q: %w", s, err)
	}
	if parsed <= 0 {
		return fmt.Errorf("duration: %q must be positive", s)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalJSON emits the duration as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// Timeouts holds runtime-tunable durations.
type Timeouts struct {
	BackendHealth Duration `json:"backend_health,omitempty"` // feishu-front: 后端 lastSeen 超时阈值，超过则驱逐静默后端
	// PromptTimeout is the per-prompt safety net for a stuck CLI subprocess.
	// 0 (default) disables it — the CLI exits on its own when the turn is
	// done. When set, a prompt exceeding this duration is cancelled (SIGKILL
	// on the process group) and the user sees a "请求超时" notice. Consumed
	// by claude-back and opencode-back.
	PromptTimeout Duration `json:"prompt_timeout,omitempty"`
}

// DedupConfig configures the frontend's application-layer replay guard.
// All fields optional; zero values mean "use the dispatcher's built-in
// default" (300s stale window, 5m event TTL, 1000 entry cap), so a
// config section that omits dedup entirely behaves the same as one that
// spells out the defaults. Only consumed by feishu-front; backends ignore.
type DedupConfig struct {
	// StaleWindow drops inbound messages whose create_time is older than
	// this. Go duration string ("300s", "5m"). <=0/absent → default 300s.
	StaleWindow Duration `json:"stale_window,omitempty"`
	// EventTTL is the eventIDs dedup table's TTL. Go duration string.
	// <=0/absent → default 5m.
	EventTTL Duration `json:"event_ttl,omitempty"`
	// EventMaxEntries is the eventIDs LRU hard cap. <=0/absent → default 1000.
	EventMaxEntries int `json:"event_max_entries,omitempty"`
}

// expandEnvVars replaces ${VAR} patterns in raw config bytes with env
// values. Returns an error if any referenced variable is unset or empty.
//
// The replacement value is JSON-string-escaped before splicing so a
// secret containing `"`, `\`, or control characters cannot break the
// surrounding JSON (it is always interpolated inside a JSON string
// value, since ${VAR} only appears in a string-typed config field).
func expandEnvVars(data []byte) ([]byte, error) {
	matches := envVarPattern.FindAllSubmatchIndex(data, -1)
	if matches == nil {
		return data, nil
	}

	var out []byte
	last := 0
	for _, m := range matches {
		out = append(out, data[last:m[0]]...)
		name := string(data[m[2]:m[3]])
		val, ok := os.LookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("config: env var ${%s} is unset (set it in bridge.env)", name)
		}
		if val == "" {
			return nil, fmt.Errorf("config: env var ${%s} is set but empty (check bridge.env)", name)
		}
		// JSON-escape the value so quotes/backslashes/control chars in a
		// secret do not corrupt the surrounding JSON document.
		escaped, err := json.Marshal(val)
		if err != nil {
			return nil, fmt.Errorf("config: escape env var ${%s}: %w", name, err)
		}
		// Marshal wraps the string in quotes; strip them because we are
		// splicing into an already-quoted JSON string literal.
		out = append(out, escaped[1:len(escaped)-1]...)
		last = m[1]
	}
	return append(out, data[last:]...), nil
}

// Load reads the config file at path and returns a validated *Config.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded, err := expandEnvVars(raw)
	if err != nil {
		return nil, fmt.Errorf("expand env: %w", err)
	}

	// DisallowUnknownFields so a typo'd key (e.g. "max_concurent") is
	// rejected rather than silently ignored — silent ignore plus
	// applyDefaults makes operators believe the config took effect.
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(expanded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	applyDefaults(&cfg, path)
	if err := validate(&cfg); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	return &cfg, nil
}
