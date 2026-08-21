package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeValidateConfig writes body to a temp config file and returns its path.
// Shared by the miniagent enum validation tests (separate from config_test.go's
// writeConfig to keep this file standalone).
func writeValidateConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestValidateMiniAgentMode_FieldRemoved verifies miniagent.mode is no longer
// a config key: miniagent v5.0.0 removed -mode entirely, so the bridge dropped
// the field. LoadMiniAgentBack strict-decodes the owned miniagent section, so
// an old config still carrying "mode" fails fast with an explicit
// unknown-field error rather than silently ignoring a stale safety-posture
// knob. Operators must delete the key.
func TestValidateMiniAgentMode_FieldRemoved(t *testing.T) {
	body := `{"miniagent":{"mode":"default"}}`
	_, err := LoadMiniAgentBack(writeValidateConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "unknown field \"mode\"") {
		t.Fatalf("want unknown-field error for removed miniagent.mode, got %v", err)
	}
}

// TestValidateMiniAgentThinking_RejectsBad covers the miniagent.thinking enum
// guard. The allowed set mirrors OMP.thinking_level minus "auto" (miniagent v3
// has no "auto" thinking).
func TestValidateMiniAgentThinking_RejectsBad(t *testing.T) {
	cases := []struct {
		name     string
		thinking string
		ok       bool
	}{
		{"off", `"off"`, true},
		{"minimal", `"minimal"`, true},
		{"low", `"low"`, true},
		{"medium", `"medium"`, true},
		{"high", `"high"`, true},
		{"xhigh", `"xhigh"`, true},
		{"max", `"max"`, true},
		{"empty (explicit clear)", `""`, true},
		{"bad auto", `"auto"`, false},
		{"bad ultra", `"ultra"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"miniagent":{"thinking":` + c.thinking + `}}`
			_, err := LoadMiniAgentBack(writeValidateConfig(t, body))
			if c.ok && err != nil {
				t.Fatalf("thinking=%s: want accept, got err: %v", c.thinking, err)
			}
			if !c.ok && (err == nil || !strings.Contains(err.Error(), "miniagent.thinking")) {
				t.Fatalf("thinking=%s: want err containing \"miniagent.thinking\", got %v", c.thinking, err)
			}
		})
	}
}

// TestValidateMiniAgentDefaults_Pass confirms applyDefaults-populated values
// (no miniagent block at all) load cleanly through validate — i.e. the enum
// switches accept the defaults.
func TestValidateMiniAgentDefaults_Pass(t *testing.T) {
	// An empty config gets the full defaults treatment, including
	// MiniAgent.Thinking="off".
	if _, err := LoadMiniAgentBack(writeValidateConfig(t, `{}`)); err != nil {
		t.Fatalf("empty config with miniagent defaults must load: %v", err)
	}
}
