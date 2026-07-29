package opencodebridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencode"
	"github.com/justphantom/lark-bridge/internal/router"
)

// replayOpencodeEvents loads a testdata fixture, parses each line via
// opencode.ParseEvent, and returns the resulting Event slice.
func replayOpencodeEvents(t *testing.T, name string) []opencode.Event {
	t.Helper()
	path := filepath.Join("testdata", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var out []opencode.Event
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		evs, err := opencode.ParseEvent(line)
		if err != nil {
			t.Fatalf("ParseEvent(%q): %v", line, err)
		}
		out = append(out, evs...)
	}
	return out
}

// TestReplay_SingleStepTurn drives streamRun with the captured
// `replay-singlestep.jsonl` stream and asserts reply/usage. §5 of the
// refactor plan requires every CLI backend to have at least one such replay
// test.
func TestReplay_SingleStepTurn(t *testing.T) {
	events := replayOpencodeEvents(t, "replay-singlestep.jsonl")
	ch := make(chan opencode.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)

	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{StateDir: t.TempDir()},
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", ch, "", nil)
	if res.Err != nil {
		t.Fatalf("streamRun: %v", res.Err)
	}
	if !strings.Contains(res.Reply, "answer body") {
		t.Errorf("reply = %q, want contains 'answer body'", res.Reply)
	}
	if res.InputTokens != 1000 || res.OutputTokens != 500 {
		t.Errorf("input/output tokens = %d/%d, want 1000/500", res.InputTokens, res.OutputTokens)
	}
	if res.CacheRead != 300 || res.CacheWrite != 50 {
		t.Errorf("cache read/write = %d/%d, want 300/50", res.CacheRead, res.CacheWrite)
	}
	if res.CostUSD != 0.02 {
		t.Errorf("costUSD = %v, want 0.02", res.CostUSD)
	}
	if res.Steps != 1 {
		t.Errorf("steps = %d, want 1", res.Steps)
	}
}
