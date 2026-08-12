// Package config loads and validates bridge configuration from a JSON file.
//
// The merged Config is the union of per-backend settings plus the IPC
// fields (BackendID/FrontendURL/RouterPath) added by the 1-frontend/N-backend
// split. Each of the binaries reads only
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
	"regexp"
	"runtime"
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
	// IPC TLS（仅 feishu-front 用）：ipc_addr 绑定非 loopback 地址时必须配置
	// cert/key，否则明文 HTTP 上的 bearer 可被同网段嗅探冒用（M10-1）。启用后
	// 后端的 frontend_url 需使用 https:// scheme。ClientCAFile 可选：配置后
	// 要求并校验后端客户端证书（mTLS）。
	IPCTLSCertFile     string `json:"ipc_tls_cert_file,omitempty"`
	IPCTLSKeyFile      string `json:"ipc_tls_key_file,omitempty"`
	IPCTLSClientCAFile string `json:"ipc_tls_client_ca_file,omitempty"`
	// 后端侧 TLS 客户端配置（各 back / monitor 用，frontend_url 为 https:// 时）：
	// IPCTLSCAFile 信任前端（可能自签）证书的 CA；IPCTLSClientCertFile/KeyFile
	// 在前端启用 mTLS（ipc_tls_client_ca_file）时提供客户端证书。
	IPCTLSCAFile         string `json:"ipc_tls_ca_file,omitempty"`
	IPCTLSClientCertFile string `json:"ipc_tls_client_cert_file,omitempty"`
	IPCTLSClientKeyFile  string `json:"ipc_tls_client_key_file,omitempty"`
	RouterPath           string `json:"router_path,omitempty"` // router 持久化文件路径（前后端共用）

	// —— 后端运行时：各后端按需 ——
	StatusMonitor StatusMonitor `json:"status_monitor,omitempty"` // status-monitor 用
	MiniAgent     MiniAgent     `json:"miniagent,omitempty"`      // miniagent-back 用

	// —— 日志：共用 ——
	LogLevel       string `json:"log_level"`
	LogOutput      string `json:"log_output,omitempty"`
	LogFormat      string `json:"log_format,omitempty"`
	LogDebugRedact bool   `json:"log_debug_redact,omitempty"` // opencode 源有

	StateDir string   `json:"state_dir,omitempty"`
	Timeouts Timeouts `json:"timeouts,omitempty"` // 两源并集

	// —— Stream archive redaction ——
	// StreamArchiveRedact enables field-level redaction of sensitive content
	// (prompt, text, content, input, output, file_text) in stream archives.
	// It is a *bool so the zero value (nil) can mean "unset → default ON": an
	// operator who omits the field gets redaction, while an explicit
	// stream_archive_redact: false disables it. A plain bool cannot tell
	// "omitted" from "explicit false" (both unmarshal to false), so the earlier
	// "force true when false" default made the opt-out impossible. When
	// enabled, each NDJSON line written to the archive is parsed and sensitive
	// fields are replaced with "[REDACTED]". Unparseable lines pass through
	// verbatim. Read the resolved value via RedactStreams().
	StreamArchiveRedact *bool `json:"stream_archive_redact,omitempty"`

	// —— 防重放：feishu-front 用，后端忽略 ——
	Dedup DedupConfig `json:"dedup,omitempty"`

	// —— 渲染：feishu-front 用，后端忽略 ——
	Renderer Renderer `json:"renderer,omitempty"`

	// —— 文件上传：feishu-front 用，后端忽略 ——
	// 仅有该段时才放开 file-type 消息；未配置 → 文件消息照旧被拒。
	FileConvert FileConvert `json:"file_convert,omitempty"`
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
	// is the default -workdir. Required since v3 -mode default needs a workdir.
	// [P1: required, enforced in cmd/miniagent-back/main.go]
	WorkspaceRoot string `json:"workspace_root,omitempty"`
	// Stream enables -stream (SSE reasoning_delta → live 思考区). Default false.
	Stream bool `json:"stream,omitempty"`
	// MaxIterations caps one turn's LLM-call count (-max-iterations). <=0 → 20.
	MaxIterations int `json:"max_iterations,omitempty"`
	// Mode is the permission mode (-mode): "default" (write confined to workdir
	// + shell rejects 11 privilege escalators) or "auto" (unrestricted). Default
	// "default" (applyDefaults). v3 replaced -confine. [P2]
	Mode string `json:"mode,omitempty"`
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
	// review. <=0/absent → 100.
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
	cfg, _, err := LoadWithWarnings(path)
	return cfg, err
}

// LoadWithWarnings is Load plus non-fatal operator warnings (low-20): today
// the only one is "config file is group/other-readable AND carries plaintext
// secrets" — deploy.sh chmods 600, but a hand-placed file gets a loud warn
// instead of silent exposure. Callers should log each warning at startup.
func LoadWithWarnings(path string) (*Config, []string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	warnings := secretPermWarnings(path, raw)
	expanded, err := expandEnvVars(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("expand env: %w", err)
	}

	// DisallowUnknownFields so a typo'd key (e.g. "max_concurent") is
	// rejected rather than silently ignored — silent ignore plus
	// applyDefaults makes operators believe the config took effect.
	// D5 deployment constraint: this strictness means a config written for
	// a NEWER binary (with added keys) is rejected by an older one — all
	// binaries sharing one config file must be deployed at the same version
	// (the deploy script already ships them together).
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(expanded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, nil, fmt.Errorf("parse json: %w", err)
	}

	applyDefaults(&cfg, path)
	if err := validate(&cfg); err != nil {
		return nil, nil, fmt.Errorf("validate: %w", err)
	}
	return &cfg, warnings, nil
}

// plaintextSecretRe matches a secret-bearing JSON key whose VALUE is a
// literal (not a "${VAR}" reference): feishu_app_secret / ipc_secret /
// miniagent api_key written in cleartext.
var plaintextSecretRe = regexp.MustCompile(`"(?:feishu_app_secret|ipc_secret|api_key)"\s*:\s*"((?:[^"$]|\$[^{])[a-zA-Z0-9_-]*)"`)

// secretPermWarnings returns a warning when the config file is
// group/other-readable AND embeds at least one plaintext secret (low-20).
// Permission alone is not warned on (the file may hold only ${VAR}
// references); plaintext alone is fine when the file is 0600. Skipped on
// non-Unix platforms where Perm bits are not meaningful.
func secretPermWarnings(path string, raw []byte) []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil // the read already succeeded; a stat race is not warn-worthy
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if !plaintextSecretRe.Match(raw) {
		return nil
	}
	return []string{fmt.Sprintf("config file %s is readable by group/other (mode %04o) and contains plaintext secrets; chmod 600 or switch to ${VAR} references", path, info.Mode().Perm())}
}
