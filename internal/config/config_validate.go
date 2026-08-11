package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// promptTemplateFuncs whitelists the template functions exposed to
// file_convert.prompt_template. Kept empty for now; declaring the map
// explicitly means a future addition (e.g. a `truncate` filter) is a
// single-point edit, and the validation parse uses the same surface the
// runtime renderer will use.
var promptTemplateFuncs = template.FuncMap{}

// PromptTemplateFuncs returns the FuncMap used when parsing and rendering
// file_convert.prompt_template. Declared as a public accessor (rather than
// letting callers redeclare the map) so the validation parse in config and
// the runtime parse in cmd/feishu-front cannot drift apart.
func PromptTemplateFuncs() template.FuncMap {
	return promptTemplateFuncs
}

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
	if err := validateIPCTLS(cfg); err != nil {
		return err
	}
	// Validate component log levels.
	for comp, level := range map[string]string{
		"router":         cfg.ComponentLogLevels.Router,
		"feishu":         cfg.ComponentLogLevels.Feishu,
		"bridge":         cfg.ComponentLogLevels.Bridge,
		"dedup":          cfg.ComponentLogLevels.Dedup,
		"miniagent":      cfg.ComponentLogLevels.MiniAgent,
		"status_monitor": cfg.ComponentLogLevels.StatusMonitor,
	} {
		if level == "" || level == "debug" || level == "info" || level == "warn" || level == "error" {
			continue
		}
		return fmt.Errorf("component_log_levels.%s must be one of debug/info/warn/error, got %q", comp, level)
	}

	// MiniAgent enum fields. applyDefaults always populates Mode/Thinking, so
	// reaching validate with "" means an explicit clear; the default values
	// themselves are valid. (WorkspaceRoot/ConfigPath requireds are binary-specific
	// → enforced in cmd/miniagent-back/main.go, per this func's header comment.)
	switch cfg.MiniAgent.Mode {
	case "default", "auto", "":
	default:
		return fmt.Errorf("miniagent.mode must be one of default/auto, got %q", cfg.MiniAgent.Mode)
	}
	switch cfg.MiniAgent.Thinking {
	case "off", "minimal", "low", "medium", "high", "xhigh", "max", "":
	default:
		return fmt.Errorf("miniagent.thinking must be one of off/minimal/low/medium/high/xhigh/max, got %q", cfg.MiniAgent.Thinking)
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
	// 1h floor: anything tighter would drop normal session lifetimes (an
	// active conversation can run hours) and serve no pruning purpose.
	if d := time.Duration(cfg.Timeouts.UsageSessionTTL); d > 0 && d < time.Hour {
		return fmt.Errorf("timeouts.usage_session_ttl must be >= 1h when set, got %s", d)
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

	// Renderer tunables. Zero values are valid (means "use renderer default").
	if cfg.Renderer.MaxThinkingRunes < 0 {
		return fmt.Errorf("renderer.max_thinking_runes must be >= 0, got %d", cfg.Renderer.MaxThinkingRunes)
	}

	// FileConvert. Only enforce when Enabled; disabled sections are inert.
	if cfg.FileConvert.Enabled {
		if cfg.FileConvert.MaxFileSize < 1<<20 {
			return fmt.Errorf("file_convert.max_file_size must be >= 1MiB, got %d bytes", cfg.FileConvert.MaxFileSize)
		}
		if d := time.Duration(cfg.FileConvert.ConvertTimeout); d > 0 && d < time.Second {
			return fmt.Errorf("file_convert.convert_timeout must be >= %s when set, got %s", time.Second, d)
		}
		if d := time.Duration(cfg.FileConvert.Retention); d > 0 && d < time.Hour {
			return fmt.Errorf("file_convert.retention must be >= 1h when set, got %s", d)
		}
		// xlsx_formula_mode must be one of the supported semantics. The
		// default ("value") is filled by applyDefaults, so reaching validate
		// with an empty value means an operator explicitly cleared it.
		switch cfg.FileConvert.XlsxFormulaMode {
		case "value", "formula", "both":
		default:
			return fmt.Errorf("file_convert.xlsx_formula_mode must be one of value/formula/both, got %q", cfg.FileConvert.XlsxFormulaMode)
		}
		// pptx/xlsx caps: only reject negative limits (0 means unlimited,
		// which is the design default, so 0 is always valid).
		if cfg.FileConvert.PptxMaxSlides < 0 {
			return fmt.Errorf("file_convert.pptx_max_slides must be >= 0 (0 = unlimited), got %d", cfg.FileConvert.PptxMaxSlides)
		}
		if cfg.FileConvert.XlsxMaxSheets < 0 {
			return fmt.Errorf("file_convert.xlsx_max_sheets must be >= 0 (0 = unlimited), got %d", cfg.FileConvert.XlsxMaxSheets)
		}
		// PromptTemplate is required: no compiled-in default exists (the
		// canonical wording ships in config.example.json
		// so operators can edit it). Refuse to start so a half-configured
		// deployment cannot ship silent file uploads.
		if strings.TrimSpace(cfg.FileConvert.PromptTemplate) == "" {
			return fmt.Errorf("file_convert.prompt_template is required when file_convert.enabled is true (copy the default from config.example.json)")
		}
		// Syntax-check at config load so a typo'd template fails fast at
		// startup, not on the first upload. Variable substitution happens at
		// render time; here we only assert the template parses.
		if err := validatePromptTemplate(cfg.FileConvert.PromptTemplate); err != nil {
			return fmt.Errorf("file_convert.prompt_template: %w", err)
		}
		// PostPromptTemplate is optional: when empty, post messages degrade
		// to text-only Markdown (no image download). When non-empty, parse
		// it once here so a typo fails fast at startup, not on the first
		// post.
		if strings.TrimSpace(cfg.FileConvert.PostPromptTemplate) != "" {
			if err := validatePromptTemplate(cfg.FileConvert.PostPromptTemplate); err != nil {
				return fmt.Errorf("file_convert.post_prompt_template: %w", err)
			}
		}
		// XlsxPromptTemplate is optional (empty → fall back to the generic
		// prompt_template). When set, parse-check it now so a typo fails at
		// startup, not on the first xlsx upload.
		if strings.TrimSpace(cfg.FileConvert.XlsxPromptTemplate) != "" {
			if err := validatePromptTemplate(cfg.FileConvert.XlsxPromptTemplate); err != nil {
				return fmt.Errorf("file_convert.xlsx_prompt_template: %w", err)
			}
		}
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
		if err := os.MkdirAll(abs, 0o750); err != nil {
			return fmt.Errorf("%s: failed to create directory: %w", label, err)
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("%s: path is not a directory: %s", label, abs)
	}
	return nil
}

// validateIPCTLS checks the IPC TLS triple (M10-1): cert/key must be paired,
// a client CA requires the pair, every named file must exist, and a
// non-loopback ipc_addr without TLS is rejected outright — plaintext HTTP on
// a routable address exposes the shared bearer to sniffing and full
// impersonation. ipc_addr is only consumed by feishu-front, so the loopback
// rule keys off it being set.
func validateIPCTLS(cfg *Config) error {
	cert, key, ca := cfg.IPCTLSCertFile, cfg.IPCTLSKeyFile, cfg.IPCTLSClientCAFile
	if (cert == "") != (key == "") {
		return fmt.Errorf("ipc_tls_cert_file and ipc_tls_key_file must be set together (cert=%q key=%q)", cert, key)
	}
	if ca != "" && cert == "" {
		return fmt.Errorf("ipc_tls_client_ca_file requires ipc_tls_cert_file/ipc_tls_key_file")
	}
	for name, path := range map[string]string{
		"ipc_tls_cert_file":        cert,
		"ipc_tls_key_file":         key,
		"ipc_tls_client_ca_file":   ca,
		"ipc_tls_ca_file":          cfg.IPCTLSCAFile,
		"ipc_tls_client_cert_file": cfg.IPCTLSClientCertFile,
		"ipc_tls_client_key_file":  cfg.IPCTLSClientKeyFile,
	} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if (cfg.IPCTLSClientCertFile == "") != (cfg.IPCTLSClientKeyFile == "") {
		return fmt.Errorf("ipc_tls_client_cert_file and ipc_tls_client_key_file must be set together")
	}
	if cfg.IPCAddr != "" && !isLoopbackIPCAddr(cfg.IPCAddr) && cert == "" {
		return fmt.Errorf("ipc_addr %q is non-loopback: ipc_tls_cert_file/ipc_tls_key_file are required (bearer would cross the network in cleartext)", cfg.IPCAddr)
	}
	return nil
}

// isLoopbackIPCAddr mirrors feishufront.isLoopbackAddr (config cannot import
// the frontend package): empty host (":6060") binds ALL interfaces and is
// therefore NOT loopback.
func isLoopbackIPCAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

// validatePromptTemplate parses t with the same FuncMap the runtime renderer
// will use, without executing it. Returns the parse error verbatim so the
// operator sees the exact line/column. Called once at config Load time so a
// broken template fails fast at startup instead of on the first upload.
func validatePromptTemplate(t string) error {
	if _, err := template.New("file_convert.prompt_template").Funcs(promptTemplateFuncs).Parse(t); err != nil {
		return err
	}
	return nil
}
