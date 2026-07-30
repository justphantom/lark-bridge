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
	OMP           OMP           `json:"omp,omitempty"`            // omp-back 用
	DeployMonitor DeployMonitor `json:"deploy_monitor,omitempty"` // deploy-monitor 用
	StatusMonitor StatusMonitor `json:"status_monitor,omitempty"` // status-monitor 用
	MiniAgent     MiniAgent     `json:"miniagent,omitempty"`      // miniagent-back 用

	// —— 日志：共用 ——
	LogLevel           string            `json:"log_level"`
	LogOutput          string            `json:"log_output,omitempty"`
	LogFormat          string            `json:"log_format,omitempty"`
	ComponentLogLevels ComponentLogLevel `json:"component_log_levels,omitempty"` // opencode 源有
	LogDebugRedact     bool              `json:"log_debug_redact,omitempty"`     // opencode 源有

	StateDir string   `json:"state_dir,omitempty"`
	Timeouts Timeouts `json:"timeouts,omitempty"` // 两源并集

	// —— Stream archive redaction ——
	// StreamArchiveRedact enables field-level redaction of sensitive content
	// (prompt, text, content, input, output, file_text) in stream archives.
	// Default false (keep existing behavior). When enabled, each NDJSON line
	// written to the archive is parsed and sensitive fields are replaced with
	// "[REDACTED]". Unparseable lines pass through verbatim.
	StreamArchiveRedact bool `json:"stream_archive_redact,omitempty"`

	// —— 防重放：feishu-front 用，后端忽略 ——
	Dedup DedupConfig `json:"dedup,omitempty"`

	// —— 渲染：feishu-front 用，后端忽略 ——
	Renderer Renderer `json:"renderer,omitempty"`

	// —— 文件上传：feishu-front 用，后端忽略 ——
	// 仅有该段时才放开 file-type 消息；未配置 → 文件消息照旧被拒。
	FileConvert FileConvert `json:"file_convert,omitempty"`
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
// Opencode holds settings for the opencode-back CLI subprocess mode. The
// bridge shells out to the `opencode` binary per turn via `opencode run`.
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

// OMP holds settings for the local Oh My Pi (omp) CLI subprocess that acts
// as the agent backend. The omp-back binary shells out to the `omp` binary
// per turn via `omp -p --mode json` and reads an NDJSON event flow from
// stdout. OMP's approval/thinking/model axes mirror claude's
// permission/effort/model (there is no agent concept), so the picker
// command set is /perm /thinking /model.
type OMP struct {
	// CLIPath is the path to the omp binary (default "omp").
	CLIPath string `json:"cli_path,omitempty"`
	// DefaultDirectory is the base dir for per-chat session working dirs.
	DefaultDirectory string `json:"default_directory,omitempty"`
	// MaxConcurrent caps parallel omp subprocesses (default 4).
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// StreamHistory caps how many recent per-run raw NDJSON captures are
	// kept under {state_dir}/streams. <=0/unset -> 50. The archive is
	// best-effort and stores lines verbatim (no redaction).
	StreamHistory int `json:"stream_history,omitempty"`
	// AppendSystemPrompt is passed verbatim as --append-system-prompt.
	AppendSystemPrompt string `json:"append_system_prompt,omitempty"`
	// ApprovalMode is the default --approval-mode. Empty defaults to "write"
	// (≈ claude acceptEdits): the CLI's "always-ask" mode prompts
	// interactively, which stalls the non-interactive -p stream; "yolo"
	// (≈ bypassPermissions) auto-executes dangerous tool calls and is left
	// as an explicit operator opt-in.
	ApprovalMode string `json:"approval_mode,omitempty"`
	// ThinkingLevel is the default --thinking level (e.g. "auto"). Empty
	// defaults to "auto".
	ThinkingLevel string `json:"thinking_level,omitempty"`
	// ModelOptions is the static fallback for the /model picker card, used
	// when the dynamic `omp models --json` fetch fails or returns nothing.
	// nil/unset leaves the picker with dynamic options only (plus the
	// custom-input box). Model availability is deployment-dependent, so no
	// default list is compiled in.
	ModelOptions []string `json:"model_options,omitempty"`
	// ApprovalOptions lists the modes offered in the interactive /perm
	// picker card. nil/unset -> [always-ask, write, yolo].
	ApprovalOptions []string `json:"approval_options,omitempty"`
	// ThinkingOptions lists the levels offered in the interactive /thinking
	// picker card. nil/unset -> [off, minimal, low, medium, high, xhigh,
	// max, auto].
	ThinkingOptions []string `json:"thinking_options,omitempty"`
	// ModelListTimeout bounds the `omp models --json` fork that backs the
	// dynamic /model picker. The subcommand fetches the provider catalog
	// over the network and was measured at ~137s on omp/17.1.8, so the
	// default "300s" gives headroom. The picker's outer cap
	// (bridgebase.listFnTimeout, also 300s) caps any value above that, so
	// setting this lower makes omp fail fast rather than wait the full
	// budget. Empty/unset -> 300s.
	ModelListTimeout Duration `json:"model_list_timeout,omitempty"`
	// ListCacheTTL bounds how long ListModels results stay cached
	// (seconds). The catalog fetch is minutes-long, so caching makes
	// repeated /model pickers instant. 0/unset -> 3600 (1h); negative
	// disables caching entirely (every call forks).
	ListCacheTTL int `json:"list_cache_ttl,omitempty"`
	// MaxAutoRetries caps how many consecutive auto_retry_start events are
	// tolerated before the bridge aborts the turn. <=0/unset -> 3 (the
	// default). Set to a negative value to disable the limit entirely.
	MaxAutoRetries int `json:"max_auto_retries,omitempty"`
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

// StatusMonitor holds settings for the lark-status-monitor backend, which
// periodically pushes an overview card (online backends + in-flight turns) to
// every chat bound to it. The card is PATCHed in place each tick and re-sent
// only if a user deleted it.
type StatusMonitor struct {
	// Interval is the refresh period. Go duration string ("60s", "2m").
	// 0/absent → 60s.
	Interval Duration `json:"interval,omitempty"`
}

// MiniAgent holds settings for the miniagent backend. Each turn forks the
// miniagent CLI binary (github.com/justphantom/miniagent): the CLI owns the
// ReAct loop, tools, and the LLM call; the bridge does IPC + slash-command
// dispatch + per-chat Directory/ModelSpec binding via the router. This
// mirrors how the claude/opencode backends shell out to their own CLIs —
// the bridge carries no LLM code of its own beyond GET /v1/models for the
// /model picker.
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
	// Model is the model id passed as the -model flag (e.g.
	// "gpt-4o", "deepseek-chat"). Required: use ${MINIAGENT_DEFAULT_MODEL}
	// in the config (config.Load rejects an unset/empty ${VAR}); an empty
	// value makes the miniagent CLI refuse to start.
	Model string `json:"model,omitempty"`
	// SystemPrompt is prepended to every turn as the system message. Empty
	// → a concise default assistant persona in config_defaults.
	SystemPrompt string `json:"system_prompt,omitempty"`
	// MaxTokens caps one completion's output tokens. <=0/unset → 4096.
	MaxTokens int `json:"max_tokens,omitempty"`
	// StreamHistory caps how many recent per-run raw NDJSON captures
	// are kept under {stateDir}/streams/miniagent/. <=0/unset → 50.
	StreamHistory int `json:"stream_history,omitempty"`
	// WorkspaceRoot bounds read_file to paths under this directory (after
	// filepath.Clean). Empty → read_file is not registered (the LLM cannot
	// call it) and the /cd picker is disabled. Recommended: ${WORKSPACE_ROOT}
	// so it shares the same env var as claude-back / opencode-back.
	WorkspaceRoot string `json:"workspace_root,omitempty"`
}

// ComponentLogLevel configures per-component log level overrides.
type ComponentLogLevel struct {
	Router        string `json:"router,omitempty"`
	Opencode      string `json:"opencode,omitempty"`
	Omp           string `json:"omp,omitempty"`
	Feishu        string `json:"feishu,omitempty"`
	Bridge        string `json:"bridge,omitempty"`
	Dedup         string `json:"dedup,omitempty"`
	DeployMonitor string `json:"deploy_monitor,omitempty"`
	MiniAgent     string `json:"miniagent,omitempty"`
	StatusMonitor string `json:"status_monitor,omitempty"`
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
	// IdleTimeout is the per-prompt idle watchdog: if the opencode CLI
	// emits no stdout event for this duration, the subprocess is deemed
	// stuck (hung on an upstream LLM call, internal deadlock, a tool
	// waiting on stdin) and is SIGKILLed so the user is not left staring
	// at a progress card that never resolves. 0 (default) disables it.
	// Unlike PromptTimeout (total wall-clock), IdleTimeout resets on every
	// received event, so a long but active turn is never cut short.
	// Consumed by opencode-back.
	IdleTimeout Duration `json:"idle_timeout,omitempty"`
	// UsageSessionTTL bounds how long a per-session usage entry stays in
	// memory (and on disk). Entries whose LastUpdate is older than this are
	// pruned by a background sweep so the in-memory map and the persisted
	// JSON cannot grow without bound over a long-running process.
	// 0/absent → 7d. Consumed by every backend that wires a usage store.
	UsageSessionTTL Duration `json:"usage_session_ttl,omitempty"`
	// CardPatchDelay is how long the feishu-front dispatcher waits after a
	// card click before PATCHing the new card state. Feishu reverts an
	// immediate PATCH within its click-handling window (~3-5s); waiting
	// past it lets the PATCH persist. 0/absent → 5s. Only consumed by
	// feishu-front; backends ignore.
	CardPatchDelay Duration `json:"card_patch_delay,omitempty"`
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

// Renderer holds feishu-front progress-card rendering tunables. All fields
// optional; zero values keep the renderer's built-in defaults. Only consumed
// by feishu-front; backends ignore.
type Renderer struct {
	// MaxThinkingRunes caps the model's live reasoning shown in the progress
	// card's "思考中" zone — the trailing runes are kept, prefixed with
	// "… （前略）" when the trace is longer. The card is a live dashboard,
	// not a reading surface, so only the tail is useful as a "what it's doing
	// right now" hint; the full trace stays in the session archive for later
	// review. <=0/absent → 50.
	MaxThinkingRunes int `json:"max_thinking_runes,omitempty"`
}

// FileConvert enables and tunes the inbound file-message pipeline in
// feishu-front. When this section is present (even with all fields at their
// defaults), the dispatcher accepts file-type Feishu messages, downloads the
// binary via the IM resources API, converts docx→md with the built-in pure
// Go parser (no external binary required), and exposes the resulting .md
// path in the prompt text so the bound backend can Read it. When the
// section is absent, file-type messages are still rejected with the
// legacy "不支持的消息类型" notice — keeping the feature opt-in per deployment.
//
// Only consumed by feishu-front; backends ignore.
type FileConvert struct {
	// Enabled is the master switch. False by default; the dispatcher keys the
	// "accept file-type messages?" decision on Enabled rather than on the
	// section's mere presence so an operator can stage config (write the
	// block with enabled:false) without flipping the feature on.
	Enabled bool `json:"enabled,omitempty"`
	// InboxDir is the root directory the dispatcher writes uploaded files
	// and converted .md into. Layout: {InboxDir}/{chatID}/{promptID}/… .
	// Empty → {state_dir}/inbox. Created on startup with 0700 perms.
	InboxDir string `json:"inbox_dir,omitempty"`
	// MaxFileSize bounds the size of one accepted upload, in bytes. An
	// upload larger than this is rejected with a notice before any download
	// attempt, so a 1 GiB accidental attachment cannot exhaust memory.
	// <=0 → 30 MiB.
	MaxFileSize int64 `json:"max_file_size,omitempty"`
	// ConvertTimeout bounds one file conversion. <=0 → 60s. A conversion
	// exceeding this is cancelled (ctx) and surfaced as a notice.
	ConvertTimeout Duration `json:"convert_timeout,omitempty"`
	// Retention bounds how long an inbox entry stays on disk after the turn
	// that produced it completed. <=0 → 7d. A background sweep at startup
	// prunes older directories so the inbox cannot grow without bound.
	Retention Duration `json:"retention,omitempty"`
	// PromptTemplate is the Go text/template rendered into the prompt body
	// the agent backend receives when a file is uploaded. Required when
	// Enabled is true; absent/empty makes Load fail so an operator can never
	// silently ship a deployment with no instruction to the agent (the
	// default template lives in config.example.json so the
	// canonical wording stays operator-editable, never compiled in).
	//
	// Variables:
	//   {{.FileName}}  original upload name (e.g. "notes.md")
	//   {{.Path}}      absolute path of the converted .md on disk
	//   {{.UserText}}  accompanying text from the upload message (""
	//                  when the user uploaded bare); use {{if .UserText}}
	//                  ... {{end}} to omit the section entirely
	PromptTemplate string `json:"prompt_template,omitempty"`
	// PostPromptTemplate renders the prompt body for a post-type (rich text)
	// Feishu message. Optional: when empty, post messages still work but
	// degrade to plain Markdown text (no image download, no body.md) so an
	// operator who wants only single-file uploads does not need to configure
	// a second template. When set, the dispatcher materialises inline images
	// into the inbox and writes body.md, naming its absolute path here.
	//
	// Variables:
	//   {{.Path}}     absolute path of body.md (the agent's read entry point)
	//   {{.UserText}} accompanying text (rarely populated for posts; post
	//                content IS the body — kept for parity with file uploads)
	PostPromptTemplate string `json:"post_prompt_template,omitempty"`
	// XlsxPromptTemplate renders the prompt body for an xlsx upload following
	// the C paradigm (office-extract-design.md §3.2): the full data body is
	// on disk, so the prompt carries only a path plus a per-sheet schema
	// summary. Optional — when empty, xlsx uploads fall back to
	// PromptTemplate (path only, no schema) so deployments without it still
	// work.
	//
	// Variables:
	//   {{.FileName}}       original upload name (e.g. "data.xlsx")
	//   {{.Path}}           absolute path of the converted .md
	//   {{.SheetCount}}     number of sheets
	//   {{.SheetsSection}}  pre-rendered per-sheet list (column names + row
	//                       counts + caveats); one bullet per sheet
	//   {{.UserText}}       accompanying text ("")
	XlsxPromptTemplate string `json:"xlsx_prompt_template,omitempty"`

	// —— pptx 提取调优（office-extract-design.md §2 / §4.1）——
	// PptxMaxSlides caps the number of slides emitted. <=0 → unlimited, which
	// is the design default (decision Q1: extract every slide). An operator
	// with a tight LLM context budget can lower it to truncate long decks.
	PptxMaxSlides int `json:"pptx_max_slides,omitempty"`
	// PptxExtractNotes reserves a switch to also emit speaker notes
	// (ppt/notesSlides). v1 ignores it and emits slides only; the field is
	// kept so future enablement is a config flip, not a schema change.
	PptxExtractNotes bool `json:"pptx_extract_notes,omitempty"`
	// PptxTextOnly forces text-only extraction: images are dropped entirely
	// (decision 3A — no download, no reference, no placeholder). v1 always
	// behaves as text-only regardless of this value; the switch is the
	// forward hook for the day image extraction is added.
	PptxTextOnly bool `json:"pptx_text_only,omitempty"`

	// —— xlsx 提取调优（C 范式：数据本体全量落盘，prompt 仅元数据）——
	// XlsxMaxSheets caps the number of sheets emitted. <=0 → unlimited
	// (decision Q10: every sheet). The full data body is always written to
	// the inbox .md; this only bounds how many sheets survive that write.
	XlsxMaxSheets int `json:"xlsx_max_sheets,omitempty"`
	// XlsxFormulaMode selects what an xlsx cell carrying a formula yields:
	//   "value"    → the cached result (default, decision 6A)
	//   "formula"  → the formula text
	//   "both"     → "cached (formula)"
	// An empty value is normalised to "value" by applyDefaults and rejected
	// by validate if it falls outside the enum.
	XlsxFormulaMode string `json:"xlsx_formula_mode,omitempty"`
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
