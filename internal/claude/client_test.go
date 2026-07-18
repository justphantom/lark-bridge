package claude

import (
	"slices"
	"testing"

	"github.com/justphantom/lark-bridge/internal/config"
)

func TestBuildCommand_IncludesSettings(t *testing.T) {
	c := New(configForTest(), nil)

	cmd, err := c.buildCommand(RunOptions{
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
	c := New(configForTest(), nil)

	cmd, err := c.buildCommand(RunOptions{Prompt: "hi"})
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
	c := New(configForTest(), nil)
	cmd, err := c.buildCommand(RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected cmd.SysProcAttr.Setpgid == true so the process group is killable on cancel")
	}
}

func configForTest() config.Claude {
	return config.Claude{
		CLIPath:        "claude",
		PermissionMode: "acceptEdits",
		MaxConcurrent:  1,
	}
}
