package ompbridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/router"
)

// replayOmpEvents loads a testdata fixture, parses each line through the omp
// parser, and returns the resulting Event slice (lines the parser ignores,
// like turn_start's neighbours, are dropped via ok=false).
func replayOmpEvents(t *testing.T, name string) []omp.Event {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var out []omp.Event
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		ev, ok, err := omp.ParseEvent(line)
		if err != nil {
			t.Fatalf("ParseEvent(%q): %v", line, err)
		}
		if !ok {
			continue
		}
		out = append(out, ev)
	}
	return out
}

// TestReplay_SingleTurn drives streamRun with the captured
// `replay-singleturn.jsonl` stream and asserts the canonical reply/usage
// shape for omp. §5 of the refactor plan requires every CLI backend to
// have at least one such replay test; omp's NDJSON shape (session header →
// agent_start → turn_start → message_update(s) → message_end → agent_end)
// is the most failure-prone because of the cross-round text reset.
func TestReplay_SingleTurn(t *testing.T) {
	events := replayOmpEvents(t, "replay-singleturn.jsonl")
	ch := make(chan omp.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := NewWithLogger(r, closedStreamOmp{}, nil, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{StateDir: t.TempDir()},
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", ch, "", nil)
	if res.Err != nil {
		t.Fatalf("streamRun: %v", res.Err)
	}
	if !strings.Contains(res.Reply, "hi there") {
		t.Errorf("reply = %q, want contains 'hi there'", res.Reply)
	}
	if res.InputTokens != 100 || res.OutputTokens != 50 {
		t.Errorf("input/output tokens = %d/%d, want 100/50", res.InputTokens, res.OutputTokens)
	}
	if res.CostUSD != 0.0007 {
		t.Errorf("costUSD = %v, want 0.0007", res.CostUSD)
	}
	if res.Steps != 1 {
		t.Errorf("steps = %d, want 1", res.Steps)
	}
}
