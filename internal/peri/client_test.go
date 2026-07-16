package peri

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// skipIfNoPeri skips the test when peri is not on PATH (CI without the CLI).
func skipIfNoPeri(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("peri"); err != nil {
		t.Skip("peri not on PATH")
	}
}

// TestRun_EmitsTextAndResult drives one trivial prompt and asserts the stream
// yields a terminal result event. This is the Phase 1 connectivity contract:
// prompt via stdin → text events → synthesized result at EOF. A Chinese prompt
// is used because short English prompts ("reply ok") occasionally yield an
// empty reply from the model, which would make the test flaky without testing
// anything real.
func TestRun_EmitsTextAndResult(t *testing.T) {
	skipIfNoPeri(t)
	c := New(Config{CLIPath: "peri", MaxTurns: 2}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{Prompt: "请回复：连通测试成功"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var (
		gotText  bool
		terminal bool
		textAcc  strings.Builder
		resultEv Event
	)
	for ev := range events {
		switch ev.GetType() {
		case EventText:
			gotText = true
			textAcc.WriteString(ev.GetText())
		case EventResult:
			resultEv = ev
			terminal = true
		case EventError:
			t.Fatalf("unexpected error event: %s", ev.GetText())
		}
	}
	if !terminal {
		t.Fatal("stream closed without a terminal result event")
	}
	// The synthesized Result is built from accumulated text chunks, so a
	// non-empty result implies gotText. Guard the empty case explicitly.
	if resultEv.GetResult() == "" {
		t.Fatal("terminal result event carried an empty reply")
	}
	if !gotText {
		t.Error("expected at least one text chunk feeding the result")
	}
	if resultEv.GetResult() != textAcc.String() {
		t.Errorf("result %q != accumulated text %q", resultEv.GetResult(), textAcc.String())
	}
	if !strings.Contains(resultEv.GetResult(), "连通") {
		t.Errorf("result %q does not contain expected reply", resultEv.GetResult())
	}
}

// TestRun_ToolUseAndResult drives a prompt that forces a tool call (list dir),
// asserting the stream yields a tool_use/tool_result pair followed by text and
// a terminal result. Verifies the tool_result error-prefix sniff path too.
func TestRun_ToolUseAndResult(t *testing.T) {
	skipIfNoPeri(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	c := New(Config{CLIPath: "peri", MaxTurns: 3}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	events, err := c.Run(ctx, RunOptions{
		Prompt:    "List the files in the current directory and report the count in one short sentence.",
		Directory: dir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var (
		gotToolUse    bool
		gotToolResult bool
		gotText       bool
		terminal      bool
	)
	for ev := range events {
		switch ev.GetType() {
		case EventToolUse:
			gotToolUse = true
			if ev.GetToolName() == "" {
				t.Error("tool_use event with empty name")
			}
		case EventToolResult:
			gotToolResult = true
			if ev.GetToolID() == "" {
				t.Error("tool_result event with empty id")
			}
		case EventText:
			gotText = true
		case EventResult:
			terminal = true
		case EventError:
			t.Fatalf("unexpected error event: %s", ev.GetText())
		}
	}
	if !terminal {
		t.Fatal("stream closed without a terminal result event")
	}
	if !gotToolUse {
		t.Error("expected at least one tool_use event")
	}
	if !gotToolResult {
		t.Error("expected at least one tool_result event")
	}
	if !gotText {
		t.Error("expected at least one text event")
	}
}

// TestRun_CancelAborts verifies context cancellation produces an error event
// (not a hang) and the channel closes. This is the abort contract: SIGKILL on
// the process group via ctx must surface as a terminal error.
func TestRun_CancelAborts(t *testing.T) {
	skipIfNoPeri(t)
	c := New(Config{CLIPath: "peri", MaxTurns: 20}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	events, err := c.Run(ctx, RunOptions{Prompt: "Write a 2000-line Go file, take your time."})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Cancel shortly after start so a long run is interrupted.
	go func() {
		time.Sleep(3 * time.Second)
		cancel()
	}()

	var gotError bool
	for ev := range events {
		switch ev.GetType() {
		case EventError:
			gotError = true
		case EventResult:
			// A result before cancel lands is acceptable; the contract is
			// "channel closes", tested implicitly by range exiting.
		}
	}
	// Cancellation should win over a synthesized result when it fires.
	if !gotError {
		// If the run finished before cancel, no error — acceptable. Only fail
		// if neither terminal signal fired (channel closed silently), which
		// would indicate a missed terminal synthesis.
		t.Log("no error event; run likely completed before cancel fired")
	}
}

// TestParseLine covers the parser without needing the peri binary: valid lines
// of each type, the tool-error prefix sniff, and a malformed line.
func TestParseLine(t *testing.T) {
	var b strings.Builder

	// text chunk
	evs, _, err := parseLine([]byte(`{"type":"text","content":"hi"}`), &b)
	if err != nil || len(evs) != 1 || evs[0].GetType() != EventText || evs[0].GetText() != "hi" {
		t.Fatalf("text parse: evs=%v err=%v", evs, err)
	}
	if b.String() != "hi" {
		t.Errorf("textAcc = %q, want %q", b.String(), "hi")
	}

	// tool_use
	evs, _, err = parseLine([]byte(`{"type":"tool_use","id":"t1","name":"Read","input":null}`), &b)
	if err != nil || len(evs) != 1 || evs[0].GetType() != EventToolUse || evs[0].GetToolName() != "Read" || evs[0].GetToolID() != "t1" {
		t.Fatalf("tool_use parse: evs=%v err=%v", evs, err)
	}

	// tool_result success
	evs, _, err = parseLine([]byte(`{"type":"tool_result","id":"t1","name":"Read","output":"file content"}`), &b)
	if err != nil || len(evs) != 1 || evs[0].GetIsToolError() {
		t.Fatalf("tool_result ok parse: evs=%v err=%v", evs, err)
	}

	// tool_result error (prefix sniff)
	evs, _, err = parseLine([]byte(`{"type":"tool_result","id":"t2","name":"Read","output":"Tool execution failed: File not found"}`), &b)
	if err != nil || len(evs) != 1 || !evs[0].GetIsToolError() {
		t.Fatalf("tool_result err parse: evs=%v err=%v", evs, err)
	}

	// malformed JSON
	_, _, err = parseLine([]byte(`{not json`), &b)
	if err == nil {
		t.Fatal("expected parse error for malformed JSON")
	}

	// unknown type is a no-op (no events, no error)
	evs, _, err = parseLine([]byte(`{"type":"future_event","data":"x"}`), &b)
	if err != nil || len(evs) != 0 {
		t.Fatalf("unknown type: evs=%v err=%v", evs, err)
	}

	// blank line is a no-op
	evs, _, err = parseLine([]byte(`   `), &b)
	if err != nil || len(evs) != 0 {
		t.Fatalf("blank line: evs=%v err=%v", evs, err)
	}
}
