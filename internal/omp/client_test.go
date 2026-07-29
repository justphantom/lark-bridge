//go:build linux || darwin

package omp

import (
	"context"
	"slices"
	"testing"

	"github.com/justphantom/lark-bridge/internal/log"
)

// argAfter returns the value following flag in args, or "" if flag is absent
// or has no value.
func argAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

// TestBuildCommand_BaseFlags verifies the always-on flags (-p / --mode json /
// --approval-mode / --thinking) and the positional prompt.
func TestBuildCommand_BaseFlags(t *testing.T) {
	c := New(Options{CLIPath: "omp", Logger: log.Nop()})
	cmd, err := c.buildCommand(context.Background(), RunOptions{
		Prompt:        "hi",
		ApprovalMode:  "write",
		ThinkingLevel: "auto",
	})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	for _, want := range []string{"-p", "--mode", "json", "--approval-mode", "--thinking"} {
		if !slices.Contains(cmd.Args, want) {
			t.Errorf("missing %q in args=%v", want, cmd.Args)
		}
	}
	if got := argAfter(cmd.Args, "--approval-mode"); got != "write" {
		t.Errorf("--approval-mode value = %q, want write", got)
	}
	if got := argAfter(cmd.Args, "--thinking"); got != "auto" {
		t.Errorf("--thinking value = %q, want auto", got)
	}
	// Prompt is the last positional arg.
	if cmd.Args[len(cmd.Args)-1] != "hi" {
		t.Errorf("last arg = %q, want prompt", cmd.Args[len(cmd.Args)-1])
	}
}

// TestBuildCommand_AppendSystemPrompt verifies --append-system-prompt is set
// only when the client carries one.
func TestBuildCommand_AppendSystemPrompt(t *testing.T) {
	c := New(Options{CLIPath: "omp", AppendSystemPrompt: "be brief", Logger: log.Nop()})
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi", ApprovalMode: "write", ThinkingLevel: "auto"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if got := argAfter(cmd.Args, "--append-system-prompt"); got != "be brief" {
		t.Errorf("--append-system-prompt value = %q, want %q", got, "be brief")
	}

	c2 := New(Options{CLIPath: "omp", Logger: log.Nop()})
	cmd2, _ := c2.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if slices.Contains(cmd2.Args, "--append-system-prompt") {
		t.Errorf("did not expect --append-system-prompt when unset, args=%v", cmd2.Args)
	}
}

// TestBuildCommand_ResumeAndModel verifies --resume / --model / --cwd appear
// only when their RunOptions fields are non-empty, and that --cwd mirrors the
// working dir (§5.2).
func TestBuildCommand_ResumeAndModel(t *testing.T) {
	c := New(Options{CLIPath: "omp", Logger: log.Nop()})
	cmd, err := c.buildCommand(context.Background(), RunOptions{
		Prompt:        "hi",
		SessionID:     "sess-1",
		Model:         "glm-5.2",
		Directory:     "/tmp/proj",
		ApprovalMode:  "write",
		ThinkingLevel: "auto",
	})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if got := argAfter(cmd.Args, "--resume"); got != "sess-1" {
		t.Errorf("--resume value = %q, want sess-1", got)
	}
	if got := argAfter(cmd.Args, "--model"); got != "glm-5.2" {
		t.Errorf("--model value = %q, want glm-5.2", got)
	}
	if got := argAfter(cmd.Args, "--cwd"); got != "/tmp/proj" {
		t.Errorf("--cwd value = %q, want /tmp/proj", got)
	}
	if cmd.Dir != "/tmp/proj" {
		t.Errorf("cmd.Dir = %q, want /tmp/proj (defence in depth)", cmd.Dir)
	}

	// Omitted when unset.
	cmd2, _ := c.buildCommand(context.Background(), RunOptions{Prompt: "hi", ApprovalMode: "write", ThinkingLevel: "auto"})
	for _, flag := range []string{"--resume", "--model", "--cwd"} {
		if slices.Contains(cmd2.Args, flag) {
			t.Errorf("did not expect %s when unset, args=%v", flag, cmd2.Args)
		}
	}
}

// TestBuildCommand_ToolsFlags verifies --no-tools wins over --tools, and that
// --tools is passed verbatim when NoTools is false.
func TestBuildCommand_ToolsFlags(t *testing.T) {
	c := New(Options{CLIPath: "omp", Logger: log.Nop()})

	// NoTools=true suppresses --tools even when Tools is set.
	cmd, _ := c.buildCommand(context.Background(), RunOptions{
		Prompt: "hi", Tools: "read,write", NoTools: true,
		ApprovalMode: "write", ThinkingLevel: "auto",
	})
	if !slices.Contains(cmd.Args, "--no-tools") {
		t.Errorf("expected --no-tools, args=%v", cmd.Args)
	}
	if slices.Contains(cmd.Args, "--tools") {
		t.Errorf("did not expect --tools when NoTools=true, args=%v", cmd.Args)
	}

	// Tools whitelist passed when NoTools=false.
	cmd2, _ := c.buildCommand(context.Background(), RunOptions{
		Prompt: "hi", Tools: "read,bash", NoTools: false,
		ApprovalMode: "write", ThinkingLevel: "auto",
	})
	if got := argAfter(cmd2.Args, "--tools"); got != "read,bash" {
		t.Errorf("--tools value = %q, want read,bash", got)
	}
	if slices.Contains(cmd2.Args, "--no-tools") {
		t.Errorf("did not expect --no-tools when Tools set, args=%v", cmd2.Args)
	}

	// Neither when both empty.
	cmd3, _ := c.buildCommand(context.Background(), RunOptions{
		Prompt: "hi", ApprovalMode: "write", ThinkingLevel: "auto",
	})
	for _, flag := range []string{"--no-tools", "--tools"} {
		if slices.Contains(cmd3.Args, flag) {
			t.Errorf("did not expect %s when both unset, args=%v", flag, cmd3.Args)
		}
	}
}

// TestBuildCommand_MaxTime verifies --max-time is forwarded only when set
// (the bridge normally leaves it empty per §7.2: --max-time aborts the turn
// and drops text, so ctx+ApplyGroupCancel is the hard-deadline path).
func TestBuildCommand_MaxTime(t *testing.T) {
	c := New(Options{CLIPath: "omp", Logger: log.Nop()})
	cmd, _ := c.buildCommand(context.Background(), RunOptions{
		Prompt: "hi", MaxTime: "5m",
		ApprovalMode: "write", ThinkingLevel: "auto",
	})
	if got := argAfter(cmd.Args, "--max-time"); got != "5m" {
		t.Errorf("--max-time value = %q, want 5m", got)
	}

	cmd2, _ := c.buildCommand(context.Background(), RunOptions{
		Prompt: "hi", ApprovalMode: "write", ThinkingLevel: "auto",
	})
	if slices.Contains(cmd2.Args, "--max-time") {
		t.Errorf("did not expect --max-time when unset, args=%v", cmd2.Args)
	}
}

// TestBuildCommand_SetsProcessGroup verifies the CLI runs as its own process
// group leader, so cancellation can SIGKILL the whole tree.
func TestBuildCommand_SetsProcessGroup(t *testing.T) {
	c := New(Options{CLIPath: "omp", Logger: log.Nop()})
	cmd, err := c.buildCommand(context.Background(), RunOptions{
		Prompt: "hi", ApprovalMode: "write", ThinkingLevel: "auto",
	})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected cmd.SysProcAttr.Setpgid == true so the process group is killable on cancel")
	}
}

// TestBuildCommand_EmptyCLIPathErrors verifies the defensive empty-path check
// in buildCommand. New() defaults an empty CLIPath to "omp" (so this path is
// unreachable via New in production), but a directly-constructed Client with
// an empty cliPath must still fail fast rather than exec an empty binary name.
func TestBuildCommand_EmptyCLIPathErrors(t *testing.T) {
	c := &Client{logger: log.Nop()} // cliPath intentionally empty
	if _, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"}); err == nil {
		t.Fatal("expected error when cli_path is empty")
	}
}

// TestNew_DefaultsCLIPath verifies the "omp" default when Options.CLIPath is
// empty, so main.go can construct without a config-supplied path.
func TestNew_DefaultsCLIPath(t *testing.T) {
	c := New(Options{Logger: log.Nop()})
	if c.cliPath != "omp" {
		t.Errorf("cliPath = %q, want omp", c.cliPath)
	}
}

// TestNew_ConcurrencyDefault verifies the semaphore cap defaults to 4.
func TestNew_ConcurrencyDefault(t *testing.T) {
	c := New(Options{Logger: log.Nop()})
	if got, want := cap(c.sem), 4; got != want {
		t.Errorf("sem cap = %d, want %d", got, want)
	}
	c2 := New(Options{MaxConcurrent: 2, Logger: log.Nop()})
	if got, want := cap(c2.sem), 2; got != want {
		t.Errorf("sem cap = %d, want %d", got, want)
	}
}
