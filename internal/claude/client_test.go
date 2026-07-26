//go:build linux || darwin

package claude

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestBuildCommand_IncludesSettings(t *testing.T) {
	c := New(optionsForTest())

	cmd, err := c.buildCommand(context.Background(), RunOptions{
		Prompt:       "hi",
		SettingsFile: "/home/user/.claude/kimi.json",
	})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if !slices.Contains(cmd.Args, "--settings") {
		t.Fatalf("expected --settings in args, got %v", cmd.Args)
	}
	idx := slices.Index(cmd.Args, "--settings")
	if idx+1 >= len(cmd.Args) || cmd.Args[idx+1] != "/home/user/.claude/kimi.json" {
		t.Fatalf("expected --settings to be followed by path, got %v", cmd.Args)
	}
}

func TestBuildCommand_OmitsEmptySettings(t *testing.T) {
	c := New(optionsForTest())

	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if slices.Contains(cmd.Args, "--settings") {
		t.Fatalf("did not expect --settings in args when SettingsFile is empty, got %v", cmd.Args)
	}
}

// TestBuildCommand_SetsProcessGroup verifies the CLI runs as its own process
// group leader, so cancellation can SIGKILL the whole tree (CLI + tool
// subprocesses) instead of orphaning grandchildren.
func TestBuildCommand_SetsProcessGroup(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected cmd.SysProcAttr.Setpgid == true so the process group is killable on cancel")
	}
}

// TestBuildCommand_DefaultPermissionMode locks in that an empty
// Options.PermissionMode resolves to acceptEdits on the command line: the
// CLI's own "default" mode prompts interactively and hangs under -p.
func TestBuildCommand_DefaultPermissionMode(t *testing.T) {
	c := New(Options{})
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	idx := slices.Index(cmd.Args, "--permission-mode")
	if idx < 0 || idx+1 >= len(cmd.Args) {
		t.Fatalf("expected --permission-mode in args, got %v", cmd.Args)
	}
	if cmd.Args[idx+1] != "acceptEdits" {
		t.Fatalf("permission-mode = %q, want acceptEdits", cmd.Args[idx+1])
	}
}

// TestNew_Defaults verifies every zero-valued Options field gets its
// documented default.
func TestNew_Defaults(t *testing.T) {
	c := New(Options{})
	if c.cliPath != "claude" {
		t.Errorf("cliPath = %q, want claude", c.cliPath)
	}
	if c.permissionMode != PermissionModeAcceptEdits {
		t.Errorf("permissionMode = %q, want %q", c.permissionMode, PermissionModeAcceptEdits)
	}
	if cap(c.sem) != defaultMaxConcurrent {
		t.Errorf("sem cap = %d, want %d", cap(c.sem), defaultMaxConcurrent)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("HOME unset: %v", err)
	}
	if want := filepath.Join(home, ".claude"); c.settingsDir != want {
		t.Errorf("settingsDir = %q, want %q", c.settingsDir, want)
	}
	if c.settingsTTL != time.Hour {
		t.Errorf("settingsTTL = %v, want 1h", c.settingsTTL)
	}
	if c.logger == nil {
		t.Error("logger = nil, want discard logger")
	}
}

// TestNew_NegativeTTLDisablesCache verifies the <0 TTL escape hatch reaches
// the internal "settingsTTL <= 0 disables caching" semantics.
func TestNew_NegativeTTLDisablesCache(t *testing.T) {
	c := New(Options{SettingsCacheTTL: -1})
	if c.settingsTTL >= 0 {
		t.Errorf("settingsTTL = %v, want negative (cache disabled)", c.settingsTTL)
	}
}

func optionsForTest() Options {
	return Options{
		CLIPath:        "claude",
		PermissionMode: "acceptEdits",
		MaxConcurrent:  1,
	}
}
