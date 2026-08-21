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
			t.Errorf("got %q", out)
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

// TestLoadService_SectionOwnership pins the per-service decode contract —
// the core of the config split:
//   - a service decodes ONLY its owned top-level keys (foreign sections in
//     the same union file are skipped, so a config edited for another
//     service cannot break this one);
//   - a typo'd TOP-LEVEL key is rejected by every service (known-key set is
//     the union of all service structs);
//   - a typo'd key INSIDE an owned section is still rejected
//     (DisallowUnknownFields on the filtered document).
func TestLoadService_SectionOwnership(t *testing.T) {
	// One union document carrying every service's sections.
	union := `{
		"feishu_app_id": "cli_x",
		"feishu_app_secret": "s",
		"dedup": {"stale_window": "120s"},
		"renderer": {"max_thinking_runes": 20},
		"file_convert": {"enabled": false},
		"miniagent": {"model": "m1", "stream_history": -1},
		"status_monitor": {"interval": "30s"}
	}`

	t.Run("LoadMiniAgentBack skips foreign sections", func(t *testing.T) {
		cfg, err := LoadMiniAgentBack(writeConfig(t, union))
		if err != nil {
			t.Fatalf("LoadMiniAgentBack: %v", err)
		}
		if cfg.MiniAgent.Model != "m1" || cfg.MiniAgent.StreamHistory != -1 {
			t.Errorf("miniagent fields not decoded: %+v", cfg.MiniAgent)
		}
		// status_monitor is owned here too (metrics-push interval reuse).
		if want := Duration(30 * time.Second); cfg.StatusMonitor.Interval != want {
			t.Errorf("status_monitor.interval = %v, want %v", cfg.StatusMonitor.Interval, want)
		}
		// Foreign feishu sections cannot leak: MiniAgentBackConfig has no
		// FeishuCreds/Dedup/Renderer/FileConvert fields at all — enforced at
		// compile time, not by zero-value assertions.
	})

	t.Run("LoadFeishuFront skips foreign sections", func(t *testing.T) {
		cfg, err := LoadFeishuFront(writeConfig(t, union))
		if err != nil {
			t.Fatalf("LoadFeishuFront: %v", err)
		}
		if cfg.FeishuAppID != "cli_x" {
			t.Errorf("feishu creds not decoded: %+v", cfg.FeishuCreds)
		}
		if want := Duration(120 * time.Second); cfg.Dedup.StaleWindow != want {
			t.Errorf("dedup.stale_window = %v, want %v", cfg.Dedup.StaleWindow, want)
		}
		if cfg.Renderer.MaxThinkingRunes != 20 {
			t.Errorf("renderer.max_thinking_runes = %d, want 20", cfg.Renderer.MaxThinkingRunes)
		}
	})

	t.Run("LoadStatusMonitor skips foreign sections", func(t *testing.T) {
		cfg, err := LoadStatusMonitor(writeConfig(t, union))
		if err != nil {
			t.Fatalf("LoadStatusMonitor: %v", err)
		}
		if want := Duration(30 * time.Second); cfg.StatusMonitor.Interval != want {
			t.Errorf("status_monitor.interval = %v, want %v", cfg.StatusMonitor.Interval, want)
		}
	})

	t.Run("top-level typo rejected by non-owning service", func(t *testing.T) {
		path := writeConfig(t, `{"feishu_app_id":"x","miniagen":{"model":"m"}}`)
		if _, err := LoadMiniAgentBack(path); err == nil || !strings.Contains(err.Error(), "miniagen") {
			t.Fatalf("want unknown-top-level-key error naming miniagen, got %v", err)
		}
	})

	t.Run("typo inside owned section rejected", func(t *testing.T) {
		path := writeConfig(t, `{"miniagent":{"modl":"x"}}`)
		if _, err := LoadMiniAgentBack(path); err == nil || !strings.Contains(err.Error(), "modl") {
			t.Fatalf("want unknown-field error naming modl, got %v", err)
		}
	})
}

// TestLoadDefaults covers the full pipeline including env expansion and the
// shared/frontend defaults.
func TestLoadDefaults(t *testing.T) {
	t.Setenv("FEISHU_APP_ID", "cli_test")
	t.Setenv("FEISHU_APP_SECRET", "secret")
	path := writeConfig(t, `{
		"feishu_app_id": "${FEISHU_APP_ID}",
		"feishu_app_secret": "${FEISHU_APP_SECRET}"
	}`)
	cfg, err := LoadFeishuFront(path)
	if err != nil {
		t.Fatalf("LoadFeishuFront: %v", err)
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

// TestLoadDisallowsUnknownFields verifies a typo'd top-level key is rejected
// rather than silently dropped. Without this guard the misspelled
// "backend_ld" would be ignored and backend_id would stay empty, misleading
// operators into believing the config took effect.
func TestLoadDisallowsUnknownFields(t *testing.T) {
	path := writeConfig(t, `{"backend_ld":"b1","frontend_url":"http://localhost:6060"}`)
	_, err := LoadFeishuFront(path)
	if err == nil {
		t.Fatalf("Load succeeded with unknown top-level key; want parse error")
	}
	if !strings.Contains(err.Error(), "backend_ld") {
		t.Errorf("error %q does not name the unknown key", err)
	}
}

// TestLoadBackendNoFeishuCreds verifies a backend config (no feishu creds,
// no feishu sections) loads: the backend's validation does not require
// frontend-specific fields.
func TestLoadBackendNoFeishuCreds(t *testing.T) {
	path := writeConfig(t, `{"backend_id":"b1","frontend_url":"http://localhost:6060"}`)
	cfg, err := LoadMiniAgentBack(path)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if cfg.BackendID != "b1" || cfg.FrontendURL != "http://localhost:6060" {
		t.Fatalf("backend fields not read: %+v", cfg.Core)
	}
}

// TestLoadRouterPathDefault verifies router_path defaults to
// {state_dir}/router.v5.json when omitted, so backend bindings persist across
// restarts. Also verifies an explicit router_path is preserved.
func TestLoadRouterPathDefault(t *testing.T) {
	// Omitted router_path + explicit state_dir → {state_dir}/router.v5.json.
	stateDir := t.TempDir()
	path := writeConfig(t, `{"state_dir":"`+stateDir+`"}`)
	cfg, err := LoadMiniAgentBack(path)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if want := filepath.Join(stateDir, "router.v5.json"); cfg.RouterPath != want {
		t.Errorf("default router_path = %q, want %q", cfg.RouterPath, want)
	}

	// Omitted router_path + omitted state_dir → {config_dir}/router.v5.json.
	path2 := writeConfig(t, `{}`)
	cfg2, err := LoadMiniAgentBack(path2)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	dir := filepath.Dir(path2)
	if want := filepath.Join(dir, "router.v5.json"); cfg2.RouterPath != want {
		t.Errorf("default router_path = %q, want %q", cfg2.RouterPath, want)
	}

	// Explicit router_path is preserved.
	path3 := writeConfig(t, `{"router_path":"`+filepath.Join(stateDir, "custom.json")+`"}`)
	cfg3, err := LoadMiniAgentBack(path3)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if want := filepath.Join(stateDir, "custom.json"); cfg3.RouterPath != want {
		t.Errorf("explicit router_path = %q, want %q", cfg3.RouterPath, want)
	}
}

// TestLoadStreamHistoryNegativeDisables pins D1: a negative stream_history is
// an explicit "disable archiving" signal that must survive applyDefaults
// (previously <=0 was coerced to 50, making streamarchive.NewSink's
// history<=0 disable branch unreachable). The negative value must reach the
// sink so the disable branch fires.
func TestLoadStreamHistoryNegativeDisables(t *testing.T) {
	path := writeConfig(t, `{"miniagent":{"stream_history":-1}}`)
	cfg, err := LoadMiniAgentBack(path)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if cfg.MiniAgent.StreamHistory != -1 {
		t.Errorf("stream_history = %d, want -1 (negative must disable, not coerce to 50)", cfg.MiniAgent.StreamHistory)
	}
}

// TestLoad_ValidationFailures_Core table-drives the shared (Core) validation
// rules. Run through LoadMiniAgentBack; any service loader exercises the
// same validateCore.
func TestLoad_ValidationFailures_Core(t *testing.T) {
	stateDirMissing := filepath.Join(t.TempDir(), "does", "not", "exist")
	tests := []struct {
		name string
		body string
		want string
	}{
		{"bad log level", `{"log_level":"trace"}`, "log_level"},
		{"bad log output", `{"log_output":"file"}`, "log_output"},
		{"bad log format", `{"log_format":"yaml"}`, "log_format"},
		{"state_dir missing", `{"state_dir":"` + stateDirMissing + `"}`, "state_dir"},
		{"backend_health too short", `{"timeouts":{"backend_health":"100ms"}}`, "timeouts.backend_health"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadMiniAgentBack(writeConfig(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want err containing %q, got %v", tt.want, err)
			}
		})
	}
}

// TestLoad_ValidationFailures_Feishu table-drives the frontend-owned
// validation rules via LoadFeishuFront (a backend loader SKIPS these
// sections, so they cannot fire there).
func TestLoad_ValidationFailures_Feishu(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"bad feishu log level", `{"feishu_log_level":"trace"}`, "feishu_log_level"},
		{"dedup stale_window too short", `{"dedup":{"stale_window":"100ms"}}`, "dedup.stale_window"},
		{"dedup event_ttl too short", `{"dedup":{"event_ttl":"100ms"}}`, "dedup.event_ttl"},
		{"dedup event_max_entries negative", `{"dedup":{"event_max_entries":-1}}`, "dedup.event_max_entries"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFeishuFront(writeConfig(t, tt.body))
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
	cfg, err := LoadMiniAgentBack(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
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
	_, err := LoadMiniAgentBack(writeConfig(t, `{"timeouts": {"backend_health": "1ns"}}`))
	if err == nil || !strings.Contains(err.Error(), "backend_health must be >=") {
		t.Fatalf("want err about backend_health floor, got %v", err)
	}
}

// TestLoad_DedupDefaults verifies a config without a "dedup" section leaves
// all dedup fields at zero — the dispatcher falls back to its built-in
// defaults (300s / 5m / 1000), not values filled here. This is intentional:
// only feishu-front consumes these fields, so backends must not see noise.
func TestLoad_DedupDefaults(t *testing.T) {
	cfg, err := LoadFeishuFront(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("LoadFeishuFront: %v", err)
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
	cfg, err := LoadFeishuFront(writeConfig(t, `{"dedup": {"stale_window": "120s", "event_ttl": "10m", "event_max_entries": 500}}`))
	if err != nil {
		t.Fatalf("LoadFeishuFront: %v", err)
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
			_, err := LoadFeishuFront(writeConfig(t, tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want err containing %q, got %v", tt.want, err)
			}
		})
	}
}

// TestLoadFileConvertPromptTemplateRequired verifies the
// "no compiled-in default" contract: when file_convert.enabled is true but
// prompt_template is absent/empty, Load must refuse to start so an operator
// cannot ship a deployment whose file uploads produce an empty prompt.
func TestLoadFileConvertPromptTemplateRequired(t *testing.T) {
	path := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {
			"enabled": true,
			"prompt_template": "   "
		}
	}`)
	_, err := LoadFeishuFront(path)
	if err == nil {
		t.Fatal("Load succeeded with empty prompt_template; want required-field error")
	}
	if !strings.Contains(err.Error(), "prompt_template is required") {
		t.Errorf("error %q does not name prompt_template", err)
	}
}

// TestLoadFileConvertPromptTemplateSyntaxChecked verifies a template that
// fails to parse is rejected at Load time rather than at first upload. The
// operator's typo must surface on `feishu-front` start, not 30 minutes later
// when a user actually drops a file.
func TestLoadFileConvertPromptTemplateSyntaxChecked(t *testing.T) {
	good := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {
			"enabled": true,
			"prompt_template": "path={{.FileName}}"
		}
	}`)
	if _, err := LoadFeishuFront(good); err != nil {
		t.Fatalf("well-formed template should Load fine, got: %v", err)
	}

	bad := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {
			"enabled": true,
			"prompt_template": "path={{.FileName"
		}
	}`)
	_, err := LoadFeishuFront(bad)
	if err == nil {
		t.Fatal("Load succeeded with unclosed template action; want parse error")
	}
	if !strings.Contains(err.Error(), "prompt_template") {
		t.Errorf("error %q does not name prompt_template", err)
	}
}

// TestLoadFileConvertDisabledSkipsTemplateCheck verifies that a disabled
// file_convert section ignores the template validation: a config that turns
// the feature off must not be forced to carry a template too.
func TestLoadFileConvertDisabledSkipsTemplateCheck(t *testing.T) {
	path := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {"enabled": false}
	}`)
	cfg, err := LoadFeishuFront(path)
	if err != nil {
		t.Fatalf("LoadFeishuFront: %v", err)
	}
	if cfg.FileConvert.PromptTemplate != "" {
		t.Errorf("disabled section should leave template empty, got %q", cfg.FileConvert.PromptTemplate)
	}
}

// TestLoadFileConvert_XlsxFormulaModeEnum verifies xlsx_formula_mode is
// constrained to value|formula|both. A typo must fail at Load, not surface
// later as a silent default at conversion time.
func TestLoadFileConvert_XlsxFormulaModeEnum(t *testing.T) {
	good := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {
			"enabled": true,
			"prompt_template": "p={{.Path}}",
			"xlsx_formula_mode": "both"
		}
	}`)
	cfg, err := LoadFeishuFront(good)
	if err != nil {
		t.Fatalf("both should Load fine, got: %v", err)
	}
	if cfg.FileConvert.XlsxFormulaMode != "both" {
		t.Errorf("formula mode = %q, want both", cfg.FileConvert.XlsxFormulaMode)
	}
	// Empty is normalised to "value" by applyDefaults.
	def := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {
			"enabled": true,
			"prompt_template": "p={{.Path}}"
		}
	}`)
	cfgDef, _ := LoadFeishuFront(def)
	if cfgDef.FileConvert.XlsxFormulaMode != "value" {
		t.Errorf("default formula mode = %q, want value", cfgDef.FileConvert.XlsxFormulaMode)
	}
	// An out-of-enum value is rejected.
	bad := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {
			"enabled": true,
			"prompt_template": "p={{.Path}}",
			"xlsx_formula_mode": "raw"
		}
	}`)
	if _, err := LoadFeishuFront(bad); err == nil {
		t.Fatal("Load succeeded with bogus xlsx_formula_mode; want enum error")
	}
}

// TestLoadFileConvert_XlsxPromptTemplateSyntaxChecked verifies a configured
// xlsx_prompt_template is parse-checked at Load time, mirroring the
// prompt_template contract.
func TestLoadFileConvert_XlsxPromptTemplateSyntaxChecked(t *testing.T) {
	bad := writeConfig(t, `{
		"state_dir": "`+t.TempDir()+`",
		"file_convert": {
			"enabled": true,
			"prompt_template": "p={{.Path}}",
			"xlsx_prompt_template": "{{.SheetCount"
		}
	}`)
	_, err := LoadFeishuFront(bad)
	if err == nil {
		t.Fatal("Load succeeded with unclosed xlsx_prompt_template action; want parse error")
	}
	if !strings.Contains(err.Error(), "xlsx_prompt_template") {
		t.Errorf("error %q does not name xlsx_prompt_template", err)
	}
}

// TestLoadMiniAgentStreamDefaults pins the StreamHistory retention cap default
// for miniagent (must survive a config that leaves it unset).
func TestLoadMiniAgentStreamDefaults(t *testing.T) {
	path := writeConfig(t, `{"miniagent":{}}`)
	cfg, err := LoadMiniAgentBack(path)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if cfg.MiniAgent.StreamHistory != 50 {
		t.Errorf("default miniagent stream_history = %d, want 50", cfg.MiniAgent.StreamHistory)
	}
}

// TestLoad_StatusMonitorIntervalDefaults verifies the interval default both
// for the owning binary and for miniagent-back (which reuses the section as
// its metrics-push period).
func TestLoad_StatusMonitorIntervalDefaults(t *testing.T) {
	cfgM, err := LoadStatusMonitor(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("LoadStatusMonitor: %v", err)
	}
	if want := Duration(StatusMonitorIntervalDefault); cfgM.StatusMonitor.Interval != want {
		t.Errorf("monitor default interval = %v, want %v", cfgM.StatusMonitor.Interval, want)
	}
	cfgB, err := LoadMiniAgentBack(writeConfig(t, `{}`))
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if want := Duration(StatusMonitorIntervalDefault); cfgB.StatusMonitor.Interval != want {
		t.Errorf("backend metrics interval default = %v, want %v", cfgB.StatusMonitor.Interval, want)
	}
}

// TestLoad_StreamArchiveRedact_DefaultsTrue pins the tri-state default:
// omitted field → nil → defaults to true via applyCoreDefaults,
// RedactStreams reports true. An explicit false survives and is reported
// false.
func TestLoad_StreamArchiveRedact_DefaultsTrue(t *testing.T) {
	// Omitted: defaults to true.
	p := writeConfig(t, `{}`)
	cfg, err := LoadMiniAgentBack(p)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if !cfg.RedactStreams() {
		t.Error("RedactStreams() = false when field omitted, want true")
	}

	// Explicit true.
	p = writeConfig(t, `{"stream_archive_redact": true}`)
	cfg, err = LoadMiniAgentBack(p)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if !cfg.RedactStreams() {
		t.Error("RedactStreams() = false when explicitly true, want true")
	}

	// Explicit false — must survive; operator must be able to opt out.
	p = writeConfig(t, `{"stream_archive_redact": false}`)
	cfg, err = LoadMiniAgentBack(p)
	if err != nil {
		t.Fatalf("LoadMiniAgentBack: %v", err)
	}
	if cfg.RedactStreams() {
		t.Error("RedactStreams() = true when explicitly false, want false")
	}
}

// TestLoadConfigExample validates that the repo-root config.example.json is
// loadable by EVERY service — it is the source template for deploy.sh /
// deploy-status.sh, and all three service configs are generated from the
// same union document. A broken example (e.g. an explicit "0s" duration that
// Duration.UnmarshalJSON rejects) causes every deployment command to fail at
// startup.
func TestLoadConfigExample(t *testing.T) {
	stateDir := t.TempDir()
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}

	t.Setenv("FEISHU_APP_ID", "cli_xxxxxxxxxxxxxxxx")
	t.Setenv("FEISHU_APP_SECRET", "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx")
	t.Setenv("IPC_SECRET", "deadbeef")
	t.Setenv("IPC_ADDR", "localhost:6060")
	t.Setenv("FRONTEND_URL", "http://localhost:6060")
	t.Setenv("STATE_DIR", stateDir)
	t.Setenv("MINIAGENT_API_KEY", "sk-test")
	t.Setenv("MINIAGENT_CHAT_URL", "http://localhost:8080/v1/chat/completions")
	t.Setenv("MINIAGENT_MODELS_URL", "http://localhost:8080/v1/models")
	t.Setenv("MINIAGENT_DEFAULT_MODEL", "test-model")
	t.Setenv("WORKSPACE_ROOT", t.TempDir())
	t.Setenv("PROJECT_ROOT", repoRoot)

	path := filepath.Join(repoRoot, "config.example.json")
	if _, err := LoadFeishuFront(path); err != nil {
		t.Fatalf("config.example.json failed to load for feishu-front: %v", err)
	}
	if _, err := LoadMiniAgentBack(path); err != nil {
		t.Fatalf("config.example.json failed to load for miniagent-back: %v", err)
	}
	if _, err := LoadStatusMonitor(path); err != nil {
		t.Fatalf("config.example.json failed to load for status-monitor: %v", err)
	}
}

// TestResolveConfigPath verifies the miniagent config path resolution logic.
func TestResolveConfigPath(t *testing.T) {
	// No ConfigPath, no ConfigDir, no ~/.miniagent → empty.
	t.Run("empty defaults", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		cfg := &MiniAgentBackConfig{}
		if got := ResolveConfigPath(cfg); got != "" {
			t.Errorf("empty defaults = %q, want empty", got)
		}
	})
	// ConfigDir with miniagent.json present.
	t.Run("config_dir with miniagent.json", func(t *testing.T) {
		cfgDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cfgDir, "miniagent.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write miniagent.json: %v", err)
		}
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigDir: cfgDir}}
		got := ResolveConfigPath(cfg)
		want := filepath.Join(cfgDir, "miniagent.json")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	// ConfigDir with only *-miniagent.json variant.
	t.Run("config_dir with variant", func(t *testing.T) {
		cfgDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cfgDir, "kimi-miniagent.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write variant: %v", err)
		}
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigDir: cfgDir}}
		got := ResolveConfigPath(cfg)
		want := filepath.Join(cfgDir, "kimi-miniagent.json")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	// ConfigPath explicit absolute.
	t.Run("explicit absolute", func(t *testing.T) {
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigPath: "/etc/miniagent/miniagent.json"}}
		got := ResolveConfigPath(cfg)
		if got != "/etc/miniagent/miniagent.json" {
			t.Errorf("got %q, want /etc/miniagent/miniagent.json", got)
		}
	})
	// ConfigPath relative resolved against ConfigDir.
	t.Run("relative resolved", func(t *testing.T) {
		cfgDir := t.TempDir()
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigDir: cfgDir, ConfigPath: "miniagent.json"}}
		got := ResolveConfigPath(cfg)
		want := filepath.Join(cfgDir, "miniagent.json")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	// ConfigPath with ~ prefix.
	t.Run("tilde prefix", func(t *testing.T) {
		home := t.TempDir()
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigPath: "~/.miniagent/miniagent.json"}}
		t.Setenv("HOME", home)
		got := ResolveConfigPath(cfg)
		want := filepath.Join(home, ".miniagent", "miniagent.json")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	// ConfigDir with ~ prefix.
	t.Run("tilde config_dir", func(t *testing.T) {
		home := t.TempDir()
		cfgDir := filepath.Join(home, ".miniagent")
		if err := os.MkdirAll(cfgDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(cfgDir, "miniagent.json"), []byte(`{}`), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigDir: "~/.miniagent"}}
		t.Setenv("HOME", home)
		got := ResolveConfigPath(cfg)
		want := filepath.Join(cfgDir, "miniagent.json")
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

// TestResolveConfigDir verifies the directory the /config picker scans. It must
// mirror the resolution ResolveConfigPath relies on (default ~/.miniagent,
// absolute verbatim, ~ expanded against HOME).
func TestResolveConfigDir(t *testing.T) {
	t.Run("default home", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		cfg := &MiniAgentBackConfig{}
		want := filepath.Join(home, ".miniagent")
		if got := ResolveConfigDir(cfg); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
	t.Run("absolute", func(t *testing.T) {
		cfgDir := t.TempDir()
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigDir: cfgDir}}
		if got := ResolveConfigDir(cfg); got != cfgDir {
			t.Errorf("got %q, want %q", got, cfgDir)
		}
	})
	t.Run("tilde", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		cfg := &MiniAgentBackConfig{MiniAgent: MiniAgent{ConfigDir: "~/.miniagent"}}
		want := filepath.Join(home, ".miniagent")
		if got := ResolveConfigDir(cfg); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
