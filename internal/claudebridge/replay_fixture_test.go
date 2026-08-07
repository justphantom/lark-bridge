package claudebridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// replayClaudeEvents loads a testdata fixture, parses each line through the
// claude parser, and returns the resulting Event slice (filtering lines the
// parser ignores, e.g. inert system subtypes).
func replayClaudeEvents(t *testing.T, name string) []claude.Event {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var out []claude.Event
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		evs, err := claude.ParseEvent(line)
		if err != nil {
			t.Fatalf("ParseEvent(%q): %v", line, err)
		}
		out = append(out, evs...)
	}
	return out
}

// closedStreamClaude is defined in handler_prompt_test.go (other tests in
// this package reuse it).

// TestReplay_HelloTurn drives streamRun with the captured `replay-hello.jsonl`
// stream and asserts the canonical reply/usage shape: thinking blocks stripped,
// result text is the final assistant message (not the result envelope, which
// is identical here but the bridge tracks message-id segmentation), usage
// numbers from the result event flow through to bridgebase.PromptResult.
//
// This is the safety net §5 of the refactor plan calls for: a regression in
// pump / parser / streamRun shows up here before reaching a real user.
func TestReplay_HelloTurn(t *testing.T) {
	events := replayClaudeEvents(t, "replay-hello.jsonl")
	ch := make(chan claude.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := NewWithLogger(r, closedStreamClaude{}, nil, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{StateDir: t.TempDir()},
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	b, _ := r.Lookup("c1")
	res := h.streamRun(context.Background(), "c1", "p1", ch, "fallback-model", b.Generation)
	if res.Err != nil {
		t.Fatalf("streamRun: %v", res.Err)
	}
	if !strings.Contains(res.Reply, "hello world") {
		t.Errorf("reply = %q, want 'hello world'", res.Reply)
	}
	if res.Model != "claude-sonnet-4-5" {
		t.Errorf("model = %q, want claude-sonnet-4-5 (stream value wins over spec fallback)", res.Model)
	}
	if res.SessionID != "fix-session-1" {
		t.Errorf("sessionID = %q, want fix-session-1", res.SessionID)
	}
	if res.InputTokens != 100 || res.OutputTokens != 50 {
		t.Errorf("input/output tokens = %d/%d, want 100/50", res.InputTokens, res.OutputTokens)
	}
	if res.CacheCreation != 200 || res.CacheRead != 300 {
		t.Errorf("cache creation/read = %d/%d, want 200/300", res.CacheCreation, res.CacheRead)
	}
	if res.CostUSD != 0.000123 {
		t.Errorf("costUSD = %v, want 0.000123", res.CostUSD)
	}
	if res.Steps != 1 {
		t.Errorf("steps = %d, want 1", res.Steps)
	}
}
