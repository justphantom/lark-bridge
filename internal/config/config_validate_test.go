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

// TestValidateMiniAgentMode_RejectsBad covers the miniagent.mode enum guard:
// applyDefaults fills Mode to "default" so a bad value here can only come from
// an explicit operator setting.
func TestValidateMiniAgentMode_RejectsBad(t *testing.T) {
	cases := []struct {
		name string
		mode string
		ok   bool
	}{
		{"default", `"default"`, true},
		{"auto", `"auto"`, true},
		{"empty (explicit clear)", `""`, true},
		{"bad yolo", `"yolo"`, false},
		{"bad free", `"free"`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := `{"miniagent":{"mode":` + c.mode + `}}`
			_, err := Load(writeValidateConfig(t, body))
			if c.ok && err != nil {
				t.Fatalf("mode=%s: want accept, got err: %v", c.mode, err)
			}
			if !c.ok && (err == nil || !strings.Contains(err.Error(), "miniagent.mode")) {
				t.Fatalf("mode=%s: want err containing \"miniagent.mode\", got %v", c.mode, err)
			}
		})
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
			_, err := Load(writeValidateConfig(t, body))
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
// (no miniagent block at all) load cleanly through validate — i.e. the new
// enum switches accept the defaults.
func TestValidateMiniAgentDefaults_Pass(t *testing.T) {
	// An empty config gets the full applyDefaults treatment, including
	// MiniAgent.Mode="default" and MiniAgent.Thinking="off".
	if _, err := Load(writeValidateConfig(t, `{}`)); err != nil {
		t.Fatalf("empty config with miniagent defaults must load: %v", err)
	}
}
