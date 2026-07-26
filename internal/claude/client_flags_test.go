//go:build linux || darwin

package claude

import (
	"context"
	"slices"
	"testing"
)

// flagValue returns the argument following flag, or fails the test.
func flagValue(t *testing.T, args []string, flag string) string {
	t.Helper()
	idx := slices.Index(args, flag)
	if idx < 0 || idx+1 >= len(args) {
		t.Fatalf("expected %s followed by a value in %v", flag, args)
	}
	return args[idx+1]
}

func TestBuildCommand_MaxTurnsAppendedWhenPositive(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi", MaxTurns: 7})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if got := flagValue(t, cmd.Args, "--max-turns"); got != "7" {
		t.Errorf("--max-turns = %q, want 7", got)
	}
}

func TestBuildCommand_MaxTurnsOmittedWhenZero(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if slices.Contains(cmd.Args, "--max-turns") {
		t.Errorf("did not expect --max-turns when MaxTurns is 0, got %v", cmd.Args)
	}
}

func TestBuildCommand_AllowedToolsAppendedVerbatim(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi", AllowedTools: "Bash,Read"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if got := flagValue(t, cmd.Args, "--allowedTools"); got != "Bash,Read" {
		t.Errorf("--allowedTools = %q, want Bash,Read", got)
	}
}

func TestBuildCommand_DisallowedToolsAppendedVerbatim(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi", DisallowedTools: "Write"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if got := flagValue(t, cmd.Args, "--disallowedTools"); got != "Write" {
		t.Errorf("--disallowedTools = %q, want Write", got)
	}
}

func TestBuildCommand_ToolListsOmittedWhenEmpty(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if slices.Contains(cmd.Args, "--allowedTools") || slices.Contains(cmd.Args, "--disallowedTools") {
		t.Errorf("did not expect tool list flags when empty, got %v", cmd.Args)
	}
}

func TestBuildCommand_AddDirsRepeatsFlag(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{
		Prompt:  "hi",
		AddDirs: []string{"/data/a", "/data/b"},
	})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	count := 0
	for i, a := range cmd.Args {
		if a == "--add-dir" {
			count++
			if i+1 >= len(cmd.Args) {
				t.Fatalf("--add-dir without value in %v", cmd.Args)
			}
		}
	}
	if count != 2 {
		t.Fatalf("want 2 --add-dir occurrences, got %d in %v", count, cmd.Args)
	}
	first := flagValue(t, cmd.Args, "--add-dir")
	if first != "/data/a" {
		t.Errorf("first --add-dir = %q, want /data/a", first)
	}
	if !slices.Contains(cmd.Args, "/data/b") {
		t.Errorf("missing /data/b in %v", cmd.Args)
	}
}

func TestBuildCommand_AddDirsOmittedWhenEmpty(t *testing.T) {
	c := New(optionsForTest())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if slices.Contains(cmd.Args, "--add-dir") {
		t.Errorf("did not expect --add-dir when AddDirs is empty, got %v", cmd.Args)
	}
}
