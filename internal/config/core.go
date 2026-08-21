package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Core is the configuration every binary shares: the front↔back IPC
// vocabulary (identity, endpoint, secret, TLS, router persistence), logging,
// state directory, tunable timeouts, and the stream-archive redaction
// switch. Each per-service config embeds Core, so its fields decode flat at
// the top level of the JSON document (wire format unchanged from the old
// union Config) and are reachable as cfg.<Field> via embedding promotion.
type Core struct {
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
}

// Timeouts holds runtime-tunable durations.
type Timeouts struct {
	BackendHealth Duration `json:"backend_health,omitempty"` // feishu-front: 后端 lastSeen 超时阈值，超过则驱逐静默后端
}

// applyCoreDefaults fills the shared fields' zero values. Called after the
// strict decode; env vars (expanded earlier) take precedence over these
// defaults.
func applyCoreDefaults(c *Core, cfgPath string) {
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogOutput == "" {
		c.LogOutput = "stderr"
	}
	if c.LogFormat == "" {
		c.LogFormat = "text"
	}
	if c.IPCAddr == "" {
		c.IPCAddr = "localhost:6060"
	}
	if c.StateDir == "" {
		// Default to the directory holding the config file so state
		// lands next to the config. Relative paths resolve to CWD.
		c.StateDir = filepath.Dir(cfgPath)
	}
	if c.RouterPath == "" {
		// Backend bindings (sessionID/directory/model/permission/etc.)
		// persist here; without it router.New runs in-memory and every
		// redeploy resets all bindings to defaults. This is the LEGACY
		// shared path — since the per-backend router split (R2) each backend
		// overrides it with router-<backend>.v5.json under state_dir, and
		// this value only serves as the backward-compat fallback (and as
		// the source for one-time legacy migration). Co-located with
		// state_dir so the conventional {state_dir}/router.v5.json path
		// holds.
		c.RouterPath = filepath.Join(c.StateDir, "router.v5.json")
	}
	if c.Timeouts.BackendHealth == 0 {
		c.Timeouts.BackendHealth = Duration(90 * time.Second)
	}
	// StreamArchiveRedact defaults to true (P1): NDJSON archives contain
	// prompts, file contents, and tool output that may include secrets. The
	// field is *bool, so nil (omitted) → default ON and explicit false → OFF.
	// RedactStreams() resolves the final value.
	if c.StreamArchiveRedact == nil {
		t := true
		c.StreamArchiveRedact = &t
	}
}

// validateCore checks the shared fields. Per-service required-field checks
// (feishu creds for feishu-front, workspace_root for miniagent-back, ...)
// stay in each binary's main.go.
func validateCore(c *Core) error {
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug/info/warn/error, got %q", c.LogLevel)
	}
	switch c.LogOutput {
	case "stderr", "stdout":
	default:
		return fmt.Errorf("log_output must be stderr or stdout, got %q", c.LogOutput)
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log_format must be text or json, got %q", c.LogFormat)
	}
	if err := validateIPCTLS(c); err != nil {
		return err
	}

	// StateDir writability.
	if c.StateDir != "" {
		stateDirAbs, err := filepath.Abs(c.StateDir)
		if err != nil {
			return fmt.Errorf("state_dir: failed to resolve absolute path: %w", err)
		}
		if err := ensureDir("state_dir", stateDirAbs, false); err != nil {
			return err
		}
	}

	// Timeout ranges.
	const minTunableTimeout = time.Second
	if d := time.Duration(c.Timeouts.BackendHealth); d > 0 && d < minTunableTimeout {
		return fmt.Errorf("timeouts.backend_health must be >= %s when set, got %s", minTunableTimeout, d)
	}
	return nil
}

// RedactStreams reports whether stream-archive redaction is enabled, resolving
// the tri-state StreamArchiveRedact: nil (operator left it unset) defaults to
// true; an explicit *false disables redaction. applyCoreDefaults normalizes
// nil → &true at load time, so the nil branch is a defensive fallback for any
// caller that runs before defaults are applied.
func (c *Core) RedactStreams() bool {
	return c.StreamArchiveRedact == nil || *c.StreamArchiveRedact
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
func validateIPCTLS(c *Core) error {
	cert, key, ca := c.IPCTLSCertFile, c.IPCTLSKeyFile, c.IPCTLSClientCAFile
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
		"ipc_tls_ca_file":          c.IPCTLSCAFile,
		"ipc_tls_client_cert_file": c.IPCTLSClientCertFile,
		"ipc_tls_client_key_file":  c.IPCTLSClientKeyFile,
	} {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if (c.IPCTLSClientCertFile == "") != (c.IPCTLSClientKeyFile == "") {
		return fmt.Errorf("ipc_tls_client_cert_file and ipc_tls_client_key_file must be set together")
	}
	if c.IPCAddr != "" && !isLoopbackIPCAddr(c.IPCAddr) && cert == "" {
		return fmt.Errorf("ipc_addr %q is non-loopback: ipc_tls_cert_file/ipc_tls_key_file are required (bearer would cross the network in cleartext)", c.IPCAddr)
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
