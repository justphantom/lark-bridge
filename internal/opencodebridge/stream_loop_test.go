package opencodebridge

import (
	"context"
	"testing"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/opencode"
	"github.com/hu/lark-bridge/internal/router"
)

func TestSummarizeToolInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "empty object returns empty",
			input: "{}",
			want:  "",
		},
		{
			name:  "read extracts filePath",
			input: `{"filePath":"/opt/codes/README.md"}`,
			want:  "/opt/codes/README.md",
		},
		{
			name:  "bash extracts command",
			input: `{"command":"make test","workdir":"/opt/codes"}`,
			want:  "make test",
		},
		{
			name:  "glob extracts pattern",
			input: `{"pattern":"README*","path":"/opt/codes"}`,
			want:  "README*",
		},
		{
			name:  "task extracts description over prompt",
			input: `{"description":"explore repo","prompt":"read all files","subagent_type":"Explore"}`,
			want:  "explore repo",
		},
		{
			name:  "unknown fields with a string fall back to first string value",
			input: `{"todos":[{"content":"a"}],"note":"hi"}`,
			want:  "hi",
		},
		{
			name:  "unknown fields with no string fall back to raw input",
			input: `{"todos":[{"content":"a"}],"count":3}`,
			want:  `{"todos":[{"content":"a"}],"count":3}`,
		},
		{
			name:  "MCP project (snake_case) extracted",
			input: `{"project":"lark-bridge"}`,
			want:  "lark-bridge",
		},
		{
			name:  "MCP repoPath (camelCase) extracted",
			input: `{"repoPath":"/opt/codes/lark-bridge","mode":"full"}`,
			want:  "/opt/codes/lark-bridge",
		},
		{
			name:  "non-json returned as-is",
			input: "not json",
			want:  "not json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := summarizeToolInput(tc.input); got != tc.want {
				t.Errorf("summarizeToolInput(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// eventChan buffers events into a closed channel the way a real opencode Run
// would, so streamRun can be driven directly without a subprocess.
func eventChan(events []opencode.Event) <-chan opencode.Event {
	ch := make(chan opencode.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

// parseLines turns NDJSON step lines into an event slice via the exported
// opencode.ParseEvent (opencode.Event fields are unexported).
func parseLines(t *testing.T, lines ...string) []opencode.Event {
	t.Helper()
	var out []opencode.Event
	for _, l := range lines {
		evs, err := opencode.ParseEvent(l)
		if err != nil {
			t.Fatalf("ParseEvent(%q): %v", l, err)
		}
		out = append(out, evs...)
	}
	return out
}

// TestStreamRun_AccumulatesCostAndTokensAcrossSteps verifies that a multi-step
// turn (N tool-calls steps + a terminal stop step) sums every step's tokens
// AND cost into the result. Previously only the terminal step's cost was kept,
// undercounting cost on tool-heavy turns while tokens were already summed.
func TestStreamRun_AccumulatesCostAndTokensAcrossSteps(t *testing.T) {
	const toolStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"tool-calls","tokens":{"total":800,"input":200,"output":80,"cache":{"read":400,"write":0}},"cost":0.01}}`
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":1500,"input":1000,"output":500,"cache":{"read":300,"write":50}},"cost":0.02}}`

	events := parseLines(t, toolStep, toolStep, stopStep)
	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "")

	// cost: 0.01 + 0.01 + 0.02 = 0.04 (would be 0.02 if only the terminal step counted).
	if res.costUSD != 0.04 {
		t.Errorf("costUSD = %v, want 0.04", res.costUSD)
	}
	if res.inputTokens != 1400 { // 200 + 200 + 1000
		t.Errorf("inputTokens = %v, want 1400", res.inputTokens)
	}
	if res.outputTokens != 660 { // 80 + 80 + 500
		t.Errorf("outputTokens = %v, want 660", res.outputTokens)
	}
	if res.cacheRead != 1100 { // 400 + 400 + 300
		t.Errorf("cacheRead = %v, want 1100", res.cacheRead)
	}
}

// TestStreamRun_SingleStepCostIsTerminal guards the single-step turn: no
// accumulation, the result cost equals the sole stop step's cost.
func TestStreamRun_SingleStepCostIsTerminal(t *testing.T) {
	const stopStep = `{"type":"step_finish","sessionID":"s1","part":{"type":"step_finish","reason":"stop","tokens":{"total":1500,"input":1000,"output":500,"cache":{"read":300,"write":50}},"cost":0.02}}`

	events := parseLines(t, stopStep)
	r, _ := router.New(nil, "", log.Nop())
	h := NewWithLogger(r, closedStreamOpencode{}, nil, HandlerConfig{StateDir: t.TempDir()}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")

	res := h.streamRun(context.Background(), "c1", "p1", eventChan(events), "")
	if res.costUSD != 0.02 {
		t.Errorf("costUSD = %v, want 0.02", res.costUSD)
	}
}
