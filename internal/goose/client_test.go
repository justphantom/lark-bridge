package goose

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// containsPair reports whether flag is immediately followed by val in args.
func containsPair(args []string, flag, val string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == val {
			return true
		}
	}
	return false
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}

// TestBuildCommand_NoSession assembles a one-shot invocation: --no-session is
// set, --name/--resume are absent, --max-turns is honoured.
func TestBuildCommand_NoSession(t *testing.T) {
	c := New(Config{CLIPath: "goose", MaxTurns: 7}, log.Nop())
	cmd, _, err := c.buildCommand(RunOptions{Prompt: "hi", Model: "gpt-x"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	// cmd.Args[0] is the binary; Args[1:] are the flags.
	args := cmd.Args
	if !contains(args, "--no-session") {
		t.Errorf("want --no-session when SessionName empty, args=%v", args)
	}
	if contains(args, "--resume") || contains(args, "--name") {
		t.Errorf("one-shot must not set --resume/--name, args=%v", args)
	}
	if !containsPair(args, "--max-turns", "7") {
		t.Errorf("--max-turns 7 missing, args=%v", args)
	}
	if !containsPair(args, "--model", "gpt-x") {
		t.Errorf("--model gpt-x missing, args=%v", args)
	}
	if !containsPair(args, "--output-format", "stream-json") {
		t.Errorf("--output-format stream-json missing, args=%v", args)
	}
	if !contains(args, "-q") {
		t.Errorf("-q (quiet) missing, args=%v", args)
	}
}

// TestBuildCommand_CreateSession assembles the first-turn invocation: --name
// without --resume.
func TestBuildCommand_CreateSession(t *testing.T) {
	c := New(Config{CLIPath: "goose", MaxTurns: 1}, log.Nop())
	cmd, _, err := c.buildCommand(RunOptions{Prompt: "hi", SessionName: "feishu:c1"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	args := cmd.Args
	if !containsPair(args, "--name", "feishu:c1") {
		t.Errorf("--name feishu:c1 missing, args=%v", args)
	}
	if contains(args, "--resume") {
		t.Errorf("first turn must not set --resume, args=%v", args)
	}
	if contains(args, "--no-session") {
		t.Errorf("create must not set --no-session, args=%v", args)
	}
}

// TestBuildCommand_ResumeSession assembles a continuation: --resume --name.
func TestBuildCommand_ResumeSession(t *testing.T) {
	c := New(Config{CLIPath: "goose", MaxTurns: 1}, log.Nop())
	cmd, _, err := c.buildCommand(RunOptions{Prompt: "more", SessionName: "feishu:c1", Resume: true})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	args := cmd.Args
	if !contains(args, "--resume") {
		t.Errorf("want --resume on Resume=true, args=%v", args)
	}
	if !containsPair(args, "--name", "feishu:c1") {
		t.Errorf("--name feishu:c1 missing, args=%v", args)
	}
}

// TestBuildCommand_DefaultMaxTurns: MaxTurns<=0 falls back to 1.
func TestBuildCommand_DefaultMaxTurns(t *testing.T) {
	c := New(Config{CLIPath: "goose"}, log.Nop())
	cmd, _, _ := c.buildCommand(RunOptions{Prompt: "hi"})
	if !containsPair(cmd.Args, "--max-turns", "1") {
		t.Errorf("MaxTurns<=0 want --max-turns 1, args=%v", cmd.Args)
	}
}

func skipIfNoGoose(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("goose"); err != nil {
		t.Skip("goose not on PATH")
	}
}

// TestRun_EmitsTextAndComplete is the connectivity contract: a one-shot goose
// run emits at least one text chunk and a terminal complete event carrying
// token usage. Requires goose on PATH + a working provider config.
func TestRun_EmitsTextAndComplete(t *testing.T) {
	skipIfNoGoose(t)
	c := New(Config{CLIPath: "goose", MaxTurns: 1}, log.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{Prompt: "Reply with exactly: pong"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var gotText, gotComplete bool
	for ev := range events {
		switch ev.GetType() {
		case EventText:
			gotText = true
		case EventComplete:
			gotComplete = true
			if ev.GetInputTokens() == 0 && ev.GetOutputTokens() == 0 {
				t.Errorf("complete event had zero usage tokens")
			}
		case EventError:
			t.Fatalf("unexpected error event: %s", ev.GetText())
		}
	}
	if !gotText {
		t.Error("want at least one text event")
	}
	if !gotComplete {
		t.Error("want a terminal complete event")
	}
}

// TestRun_LineSinkTeed verifies the archive sink receives the raw NDJSON
// verbatim, one line per stdout line.
func TestRun_LineSinkTeed(t *testing.T) {
	skipIfNoGoose(t)
	c := New(Config{CLIPath: "goose", MaxTurns: 1}, log.Nop())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var sink strings.Builder
	events, err := c.Run(ctx, RunOptions{Prompt: "Reply with: ok", LineSink: &sink})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for range events {
	}
	if sink.Len() == 0 {
		t.Fatal("LineSink captured no bytes")
	}
	if !strings.Contains(sink.String(), `"type":"complete"`) {
		t.Errorf("sink missing the complete line, got: %s", sink.String()[:min(200, sink.Len())])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
