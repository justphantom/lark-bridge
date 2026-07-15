package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes a JSON config body to a temp file and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestExpandEnvVars covers the ${VAR} expansion: plain, in-string, multiple,
// unset/empty rejection, and JSON-escape behaviour for secrets with quotes.
func TestExpandEnvVars(t *testing.T) {
	t.Run("no placeholders returns bytes unchanged", func(t *testing.T) {
		in := []byte(`{"key":"value"}`)
		out, err := expandEnvVars(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(out) != string(in) {
			t.Errorf("got %q, want %q", out, in)
		}
	})

	t.Run("single placeholder expanded", func(t *testing.T) {
		t.Setenv("TEST_VAR_X", "expanded-value")
		in := []byte(`{"key":"${TEST_VAR_X}"}`)
		out, err := expandEnvVars(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(out) != `{"key":"expanded-value"}` {
			t.Errorf("got %q", out)
		}
	})

	t.Run("multiple placeholders in one string", func(t *testing.T) {
		t.Setenv("VAR_A", "first")
		t.Setenv("VAR_B", "second")
		in := []byte(`{"key":"${VAR_A} and ${VAR_B}"}`)
		out, err := expandEnvVars(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(out) != `{"key":"first and second"}` {
			t.Errorf("got %q", out)
		}
	})

	t.Run("unset env var returns error", func(t *testing.T) {
		if err := os.Unsetenv("DEFINITELY_UNSET_Y7K"); err != nil {
			t.Fatalf("unset: %v", err)
		}
		in := []byte(`{"key":"${DEFINITELY_UNSET_Y7K}"}`)
		if _, err := expandEnvVars(in); err == nil {
			t.Fatal("expected error for unset var, got nil")
		}
	})

	t.Run("empty env var returns error", func(t *testing.T) {
		t.Setenv("EMPTY_VAR_X", "")
		in := []byte(`{"key":"${EMPTY_VAR_X}"}`)
		if _, err := expandEnvVars(in); err == nil {
			t.Fatal("expected error for empty var, got nil")
		}
	})

	t.Run("value with quotes is JSON-escaped", func(t *testing.T) {
		t.Setenv("QUOTED_VAR", `a"b\c`)
		in := []byte(`{"key":"${QUOTED_VAR}"}`)
		out, err := expandEnvVars(in)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Escaped value keeps the JSON valid: "a\"b\\c".
		want := `{"key":"a\"b\\c"}`
		if string(out) != want {
			t.Errorf("got %q, want %q", out, want)
		}
	})
}

func TestEnvVarPattern(t *testing.T) {
	for _, name := range []string{"VAR", "VAR_1", "_PRIVATE", "A1_B2"} {
		if !envVarPattern.MatchString("${" + name + "}") {
			t.Errorf("expected %q to match", name)
		}
	}
	for _, name := range []string{"1VAR", "VAR-1"} {
		if envVarPattern.MatchString("${" + name + "}") {
			t.Errorf("expected %q NOT to match", name)
		}
	}
}

// TestLoadDefaults covers the full pipeline including env expansion and the
// union defaults. A backend-style config (no feishu creds) loads cleanly.
func TestLoadDefaults(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "cli_test")
	t.Setenv("FEISHU_APP_SECRET", "secret")
	path := writeConfig(t, `{
		"feishu_app_id": "${FEISHU_APP_ID}",
		"feishu_app_secret": "${FEISHU_APP_SECRET}"
	}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.FeishuAppID != "cli_test" || cfg.FeishuAppSecret != "secret" {
		t.Fatalf("env expansion failed: %+v", cfg)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("default log_level = %q, want info", cfg.LogLevel)
	}
	if cfg.FeishuDomain != "feishu" {
		t.Errorf("default feishu_domain = %q, want feishu", cfg.FeishuDomain)
	}
}

// TestLoadDisallowsUnknownFields verifies a typo'd key is rejected rather
// than silently dropped. Without DisallowUnknownFields the misspelled
// "backend_ld" would be ignored and backend_id would fall back to default,
// misleading operators into believing the config took effect.
func TestLoadDisallowsUnknownFields(t *testing.T) {
	path := writeConfig(t, `{"backend_ld":"b1","frontend_url":"http://localhost:6060"}`)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load succeeded with unknown field; want parse error")
	}
	if !strings.Contains(err.Error(), "backend_ld") {
		t.Errorf("error %q does not name the unknown field", err)
	}
}

// TestLoadBackendNoFeishuCreds verifies a backend config (no feishu creds,
// no opencode section) loads: the shared validate does not require
// binary-specific fields.
func TestLoadBackendNoFeishuCreds(t *testing.T) {
	path := writeConfig(t, `{"backend_id":"b1","frontend_url":"http://localhost:6060"}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BackendID != "b1" || cfg.FrontendURL != "http://localhost:6060" {
		t.Fatalf("backend fields not read: %+v", cfg)
	}
}

// TestLoadRouterPathDefault verifies router_path defaults to
// {state_dir}/router.v5.json when omitted, so backend bindings persist across
// restarts. Also verifies an explicit router_path is preserved.
func TestLoadRouterPathDefault(t *testing.T) {
	// Omitted router_path + explicit state_dir → {state_dir}/router.v5.json.
	stateDir := t.TempDir()
	path := writeConfig(t, `{"state_dir":"`+stateDir+`"}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(stateDir, "router.v5.json"); cfg.RouterPath != want {
		t.Errorf("default router_path = %q, want %q", cfg.RouterPath, want)
	}

	// Omitted router_path + omitted state_dir → {config_dir}/router.v5.json.
	path2 := writeConfig(t, `{}`)
	cfg2, err := Load(path2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	dir := filepath.Dir(path2)
	if want := filepath.Join(dir, "router.v5.json"); cfg2.RouterPath != want {
		t.Errorf("default router_path = %q, want %q", cfg2.RouterPath, want)
	}

	// Explicit router_path is preserved.
	path3 := writeConfig(t, `{"router_path":"`+filepath.Join(stateDir, "custom.json")+`"}`)
	cfg3, err := Load(path3)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := filepath.Join(stateDir, "custom.json"); cfg3.RouterPath != want {
		t.Errorf("explicit router_path = %q, want %q", cfg3.RouterPath, want)
	}
}

// TestLoadClaudeFields verifies claude defaults and validation when a
// claude section is present.
func TestLoadClaudeFields(t *testing.T) {
	path := writeConfig(t, `{"claude":{}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Claude.CLIPath != "claude" {
		t.Errorf("default cli_path = %q, want claude", cfg.Claude.CLIPath)
	}
	if cfg.Claude.PermissionMode != "acceptEdits" {
		t.Errorf("default permission_mode = %q, want acceptEdits", cfg.Claude.PermissionMode)
	}
	if cfg.Claude.MaxConcurrent != 4 {
		t.Errorf("default max_concurrent = %d, want 4", cfg.Claude.MaxConcurrent)
	}
	if cfg.Claude.StreamHistory != 50 {
		t.Errorf("default stream_history = %d, want 50", cfg.Claude.StreamHistory)
	}
	if len(cfg.Claude.ModelOptions) != 3 || cfg.Claude.ModelOptions[0] != "haiku" {
		t.Errorf("default model_options = %v, want [haiku sonnet opus]", cfg.Claude.ModelOptions)
	}
	wantPerm := []string{"acceptEdits", "plan", "bypassPermissions"}
	if len(cfg.Claude.PermissionOptions) != 3 || cfg.Claude.PermissionOptions[0] != wantPerm[0] {
		t.Errorf("default permission_options = %v, want %v", cfg.Claude.PermissionOptions, wantPerm)
	}
	wantEffort := []string{"low", "medium", "high", "xhigh", "max"}
	if len(cfg.Claude.EffortOptions) != 5 || cfg.Claude.EffortOptions[0] != wantEffort[0] {
		t.Errorf("default effort_options = %v, want %v", cfg.Claude.EffortOptions, wantEffort)
	}
	if cfg.Claude.SettingsCacheTTL != 3600 {
		t.Errorf("default settings_cache_ttl = %d, want 3600", cfg.Claude.SettingsCacheTTL)
	}
}

// TestLoadClaudePickerOptionsOverride verifies explicit model/permission/effort
// option lists survive applyDefaults (they are nil-only-defaulted).
func TestLoadClaudePickerOptionsOverride(t *testing.T) {
	path := writeConfig(t, `{"claude":{"model_options":["a","b"],"permission_options":["plan"],"effort_options":["max"]}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Claude.ModelOptions) != 2 || cfg.Claude.ModelOptions[0] != "a" {
		t.Errorf("model_options = %v, want [a b]", cfg.Claude.ModelOptions)
	}
	if len(cfg.Claude.PermissionOptions) != 1 || cfg.Claude.PermissionOptions[0] != "plan" {
		t.Errorf("permission_options = %v, want [plan]", cfg.Claude.PermissionOptions)
	}
	if len(cfg.Claude.EffortOptions) != 1 || cfg.Claude.EffortOptions[0] != "max" {
		t.Errorf("effort_options = %v, want [max]", cfg.Claude.EffortOptions)
	}
}

// TestLoadClaudeSettingsCacheTTLDefault verifies the default settings_cache_ttl
// is applied (3600) and an explicit value survives.
func TestLoadClaudeSettingsCacheTTL(t *testing.T) {
	// Default
	path := writeConfig(t, `{"claude":{}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Claude.SettingsCacheTTL != 3600 {
		t.Errorf("default settings_cache_ttl = %d, want 3600", cfg.Claude.SettingsCacheTTL)
	}
	// Override
	path = writeConfig(t, `{"claude":{"settings_cache_ttl":120,"settings_dir":"/etc/claude"}}`)
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Claude.SettingsCacheTTL != 120 {
		t.Errorf("settings_cache_ttl = %d, want 120", cfg.Claude.SettingsCacheTTL)
	}
	if cfg.Claude.SettingsDir != "/etc/claude" {
		t.Errorf("settings_dir = %q, want /etc/claude", cfg.Claude.SettingsDir)
	}
}

// TestLoadOpencodeFields verifies opencode defaults when an opencode section is
// present.
func TestLoadOpencodeFields(t *testing.T) {
	path := writeConfig(t, `{"opencode":{}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Opencode.CLIPath != "opencode" {
		t.Errorf("default cli_path = %q, want opencode", cfg.Opencode.CLIPath)
	}
	if cfg.Opencode.MaxConcurrent != 4 {
		t.Errorf("default max_concurrent = %d, want 4", cfg.Opencode.MaxConcurrent)
	}
	if cfg.Opencode.StreamHistory != 50 {
		t.Errorf("default stream_history = %d, want 50", cfg.Opencode.StreamHistory)
	}
	if cfg.Opencode.ListCacheTTL != 3600 {
		t.Errorf("default list_cache_ttl = %d, want 3600", cfg.Opencode.ListCacheTTL)
	}
}

// TestLoadOpencodeStreamHistoryOverride ensures an explicit opencode
// stream_history survives applyDefaults.
func TestLoadOpencodeStreamHistoryOverride(t *testing.T) {
	path := writeConfig(t, `{"opencode":{"stream_history":7}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Opencode.StreamHistory != 7 {
		t.Errorf("stream_history = %d, want 7", cfg.Opencode.StreamHistory)
	}
}

// TestLoadOpencodeListCacheTTLOverride ensures an explicit list_cache_ttl
// survives applyDefaults (0 is the JSON zero value, so the test uses a
// non-zero override to prove the value is passed through).
func TestLoadOpencodeListCacheTTLOverride(t *testing.T) {
	path := writeConfig(t, `{"opencode":{"list_cache_ttl":120}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Opencode.ListCacheTTL != 120 {
		t.Errorf("list_cache_ttl = %d, want 120", cfg.Opencode.ListCacheTTL)
	}
}

// TestLoadOpencodeListCacheTTLNegativeSurvives ensures a negative
// list_cache_ttl (the documented "disable caching" sentinel) is NOT replaced
// by the 3600 default — applyDefaults only fills the zero value.
func TestLoadOpencodeListCacheTTLNegativeSurvives(t *testing.T) {
	path := writeConfig(t, `{"opencode":{"list_cache_ttl":-1}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Opencode.ListCacheTTL != -1 {
		t.Errorf("list_cache_ttl = %d, want -1 (negative must survive to disable caching)", cfg.Opencode.ListCacheTTL)
	}
}

// TestLoadStreamHistoryOverride ensures an explicit stream_history survives
// applyDefaults (the <=0 coercion only fills unset/non-positive values).
func TestLoadStreamHistoryOverride(t *testing.T) {
	path := writeConfig(t, `{"claude":{"stream_history":7}}`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Claude.StreamHistory != 7 {
		t.Errorf("stream_history = %d, want 7", cfg.Claude.StreamHistory)
	}
}

func TestValidateRejectsDefaultPermissionMode(t *testing.T) {
	path := writeConfig(t, `{"claude":{"permission_mode":"default"}}`)
	if _, err := Load(path); err == nil {
		t.Fatal(`expected error for permission_mode "default"`)
	}
}

func TestValidateBadPermissionMode(t *testing.T) {
	path := writeConfig(t, `{"claude":{"permission_mode":"bogus"}}`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for bad permission_mode")
	}
}

// TestLoad_ValidationFailures table-drives the shared validate rules.
func TestLoad_ValidationFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"bad log level", `{"log_level":"trace"}`, "log_level"},
		{"opencode negative concurrency", `{"opencode":{"max_concurrent":-1}}`, "opencode.max_concurrent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want err containing %q, got %v", tt.want, err)
			}
		})
	}
}

// TestDurationUnmarshal exercises the Duration JSON codec.
func TestDurationUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    time.Duration
		wantErr string
	}{
		{"valid 5m", `"5m"`, 5 * time.Minute, ""},
		{"valid 60s", `"60s"`, 60 * time.Second, ""},
		{"valid 250ms", `"250ms"`, 250 * time.Millisecond, ""},
		{"zero rejected", `"0"`, 0, "must be positive"},
		{"negative rejected", `"-5s"`, 0, "must be positive"},
		{"garbage rejected", `"xyz"`, 0, "parse"},
		{"non-string rejected", `5`, 0, "expect a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var d Duration
			err := d.UnmarshalJSON([]byte(tt.input))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want err containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got := time.Duration(d); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoad_TimeoutsDefaults verifies that a config without a "timeouts"
// section gets the BackendHealth default.
func TestLoad_TimeoutsDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Timeouts{
		BackendHealth: Duration(90 * time.Second),
	}
	if cfg.Timeouts != want {
		t.Fatalf("defaults = %+v, want %+v", cfg.Timeouts, want)
	}
}

// TestLoad_BackendHealthMinDuration verifies a sub-second backend_health is
// rejected so a misconfigured floor does not evict backends instantly.
func TestLoad_BackendHealthMinDuration(t *testing.T) {
	_, err := Load(writeConfig(t, `{"timeouts": {"backend_health": "1ns"}}`))
	if err == nil || !strings.Contains(err.Error(), "backend_health must be >=") {
		t.Fatalf("want err about backend_health floor, got %v", err)
	}
}

// TestLoad_PromptTimeout verifies PromptTimeout is parsed from config and
// defaults to 0 (disabled) when omitted.
func TestLoad_PromptTimeout(t *testing.T) {
	// Omitted → 0 (disabled).
	cfg, err := Load(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Timeouts.PromptTimeout != 0 {
		t.Errorf("default prompt_timeout = %v, want 0 (disabled)", cfg.Timeouts.PromptTimeout)
	}

	// Explicit value is preserved.
	cfg2, err := Load(writeConfig(t, `{"timeouts": {"prompt_timeout": "30m"}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := Duration(30 * time.Minute); cfg2.Timeouts.PromptTimeout != want {
		t.Errorf("prompt_timeout = %v, want %v", cfg2.Timeouts.PromptTimeout, want)
	}
}

// TestLoad_PromptTimeoutMinDuration verifies a sub-second prompt_timeout is
// rejected so a misconfigured value cannot kill prompts instantly.
func TestLoad_PromptTimeoutMinDuration(t *testing.T) {
	_, err := Load(writeConfig(t, `{"timeouts": {"prompt_timeout": "1ns"}}`))
	if err == nil || !strings.Contains(err.Error(), "prompt_timeout must be >=") {
		t.Fatalf("want err about prompt_timeout floor, got %v", err)
	}
}

// TestLoad_DedupDefaults verifies a config without a "dedup" section leaves
// all dedup fields at zero — the dispatcher falls back to its built-in
// defaults (300s / 5m / 1000), not values filled here. This is intentional:
// only feishu-front consumes these fields, so backends must not see noise.
func TestLoad_DedupDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Dedup.StaleWindow != 0 {
		t.Errorf("default stale_window = %v, want 0 (use dispatcher default)", cfg.Dedup.StaleWindow)
	}
	if cfg.Dedup.EventTTL != 0 {
		t.Errorf("default event_ttl = %v, want 0 (use dispatcher default)", cfg.Dedup.EventTTL)
	}
	if cfg.Dedup.EventMaxEntries != 0 {
		t.Errorf("default event_max_entries = %d, want 0 (use dispatcher default)", cfg.Dedup.EventMaxEntries)
	}
}

// TestLoad_DedupExplicit verifies explicit dedup values are parsed and kept.
func TestLoad_DedupExplicit(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{"dedup": {"stale_window": "120s", "event_ttl": "10m", "event_max_entries": 500}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := Duration(120 * time.Second); cfg.Dedup.StaleWindow != want {
		t.Errorf("stale_window = %v, want %v", cfg.Dedup.StaleWindow, want)
	}
	if want := Duration(10 * time.Minute); cfg.Dedup.EventTTL != want {
		t.Errorf("event_ttl = %v, want %v", cfg.Dedup.EventTTL, want)
	}
	if cfg.Dedup.EventMaxEntries != 500 {
		t.Errorf("event_max_entries = %d, want 500", cfg.Dedup.EventMaxEntries)
	}
}

// TestLoad_DedupValidationFailures covers the three rejection rules:
// sub-second stale_window / event_ttl and a negative event_max_entries.
func TestLoad_DedupValidationFailures(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"stale_window too small", `{"dedup": {"stale_window": "1ns"}}`, "dedup.stale_window must be >="},
		{"event_ttl too small", `{"dedup": {"event_ttl": "1ns"}}`, "dedup.event_ttl must be >="},
		{"negative max entries", `{"dedup": {"event_max_entries": -1}}`, "dedup.event_max_entries must be >="},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want err containing %q, got %v", tt.want, err)
			}
		})
	}
}
