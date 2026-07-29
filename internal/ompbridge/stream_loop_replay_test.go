package ompbridge

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/router"
)

// closedStreamOmp is a no-op agent used only to construct a Handler — the
// streamRun path is driven directly via eventChan so no real subprocess is
// needed.
type closedStreamOmp struct{}

func (closedStreamOmp) Run(context.Context, omp.RunOptions) (<-chan omp.Event, error) {
	ch := make(chan omp.Event)
	close(ch)
	return ch, nil
}
func (closedStreamOmp) IsReady(context.Context) error { return nil }

// ompEventChan buffers events into a closed channel the way a real omp Run
// would, so streamRun can be driven directly without a subprocess.
func ompEventChan(events []omp.Event) <-chan omp.Event {
	ch := make(chan omp.Event, len(events))
	for _, e := range events {
		ch <- e
	}
	close(ch)
	return ch
}

// ompParseLines turns NDJSON lines into an event slice via the exported
// omp.ParseEvent. Lines that the parser intentionally ignores (turn_start
// etc.) return ok=false and are skipped.
func ompParseLines(t *testing.T, lines ...string) []omp.Event {
	t.Helper()
	var out []omp.Event
	for _, l := range lines {
		evs, ok, err := omp.ParseEvent(l)
		if err != nil {
			t.Fatalf("ParseEvent(%q): %v", l, err)
		}
		if !ok {
			continue
		}
		out = append(out, evs)
	}
	return out
}

func newOmpReplayHandler(t *testing.T) (*Handler, *router.Router) {
	t.Helper()
	r, err := router.New("", log.Nop())
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	h := NewWithLogger(r, closedStreamOmp{}, nil, HandlerConfig{
		CoreConfig: bridgebase.CoreConfig{
			StateDir: t.TempDir(),
		},
	}, log.Nop())
	r.Bind("c1", "", t.TempDir(), "", "", "")
	return h, r
}

// TestStreamRun_TurnStartResetsTextEachRound locks in the empirically-
// discovered text accumulator boundary for omp (§3.2.6 in the refactor plan):
// a tool-call turn streams one assistant message per round — round N's
// inline-thinking preamble must not be concatenated onto round N+1's final
// answer. The boundary is EventTurnStart (not EventAgentStart, which fires
// only once per turn), so a regression here would either:
//   - leak the preamble into the reply (if reset moved to agent_start), or
//   - wrongly discard round 1's text if the reset landed on agent_end.
//
// The fixture below is a synthesised agnes-2.0-flash shape: agent_start →
// turn_start#1 → message_update(preamble) → message_end#1 → turn_start#2 →
// message_update(final answer) → message_end#2 → agent_end.
func TestStreamRun_TurnStartResetsTextEachRound(t *testing.T) {
	const sessionHdr = `{"type":"session","id":"s1","cwd":"/tmp"}`
	const agentStart = `{"type":"agent_start"}`
	const turnStart = `{"type":"turn_start"}`
	const preamble = `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"PREAMBLE_BEFORE_TOOL "}}`
	const msgEndRound1 = `{"type":"message_end","message":{"role":"assistant","stopReason":"tool-calls","usage":{"input":10,"output":20,"cacheRead":0,"cacheWrite":0,"totalTokens":30,"cost":{"total":0.001}}}}`
	const finalAnswer = `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"FINAL_ANSWER_AFTER_TOOL"}}`
	const msgEndRound2 = `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"input":50,"output":100,"cacheRead":5,"cacheWrite":0,"totalTokens":150,"cost":{"total":0.005}}}}`
	const agentEnd = `{"type":"agent_end","isTerminal":true}`

	events := ompParseLines(t, sessionHdr, agentStart, turnStart, preamble, msgEndRound1, turnStart, finalAnswer, msgEndRound2, agentEnd)
	h, _ := newOmpReplayHandler(t)

	res := h.streamRun(context.Background(), "c1", "p1", ompEventChan(events), "spec-model", nil)
	if res.Err != nil {
		t.Fatalf("streamRun: %v", res.Err)
	}
	if strings.Contains(res.Reply, "PREAMBLE_BEFORE_TOOL") {
		t.Errorf("preamble leaked into reply: %q", res.Reply)
	}
	if !strings.Contains(res.Reply, "FINAL_ANSWER_AFTER_TOOL") {
		t.Errorf("reply missing final answer: %q", res.Reply)
	}
}

// TestStreamRun_AccumulatesUsageAcrossAssistantMessageEnds pins usage
// accounting on omp: agent_end carries no telemetry, so every role=assistant
// message_end must be summed. A regression that switched to agent_end (or to
// only the terminal message_end) would under-count cacheRead here.
func TestStreamRun_AccumulatesUsageAcrossAssistantMessageEnds(t *testing.T) {
	const sessionHdr = `{"type":"session","id":"s1","cwd":"/tmp"}`
	const agentStart = `{"type":"agent_start"}`
	const turnStart = `{"type":"turn_start"}`
	const textEv = `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hi"}}`
	const msgEnd1 = `{"type":"message_end","message":{"role":"assistant","stopReason":"tool-calls","usage":{"input":10,"output":20,"cacheRead":100,"cacheWrite":0,"totalTokens":30,"cost":{"total":0.001}}}}`
	const msgEnd2 = `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"input":50,"output":100,"cacheRead":300,"cacheWrite":0,"totalTokens":150,"cost":{"total":0.005}}}}`
	const toolMsgEnd = stringWidgetMsgEnd // role=toolResult carries no usage; the parser must zero it
	const agentEnd = `{"type":"agent_end","isTerminal":true}`

	events := ompParseLines(t, sessionHdr, agentStart, turnStart, textEv, msgEnd1,
		toolMsgEnd, turnStart, textEv, msgEnd2, agentEnd)
	h, _ := newOmpReplayHandler(t)

	res := h.streamRun(context.Background(), "c1", "p1", ompEventChan(events), "spec-model", nil)
	if res.Err != nil {
		t.Fatalf("streamRun: %v", res.Err)
	}
	if res.InputTokens != 60 { // 10 + 50
		t.Errorf("inputTokens = %v, want 60", res.InputTokens)
	}
	if res.OutputTokens != 120 { // 20 + 100
		t.Errorf("outputTokens = %v, want 120", res.OutputTokens)
	}
	if res.CacheRead != 400 { // 100 + 300
		t.Errorf("cacheRead = %v, want 400 (only role=assistant message_end counts)", res.CacheRead)
	}
	if res.CostUSD != 0.006 { // 0.001 + 0.005
		t.Errorf("costUSD = %v, want 0.006", res.CostUSD)
	}
}

// stringWidgetMsgEnd is a synthesised role=toolResult message_end (e.g. the
// result of a widget tool call) — its message.usage is missing/zero, which
// the parser must handle without contaminating the accumulator.
const stringWidgetMsgEnd = `{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"ok"}]}}`

// TestStreamRun_UnknownEventTypeIsLoggedNotFatal verifies the forward-compat
// default branch: an unknown line type is logged at debug and the turn
// continues. Locks in §A.2's promise that an upstream schema addition does
// not break the bridge.
func TestStreamRun_UnknownEventTypeIsLoggedNotFatal(t *testing.T) {
	const sessionHdr = `{"type":"session","id":"s1","cwd":"/tmp"}`
	const agentStart = `{"type":"agent_start"}`
	const unknown = `{"type":"some_future_event_type","payload":{"anything":"here"}}`
	const turnStart = `{"type":"turn_start"}`
	const textEv = `{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ok"}}`
	const msgEnd = `{"type":"message_end","message":{"role":"assistant","stopReason":"stop","usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":{"total":0}}}}`
	const agentEnd = `{"type":"agent_end","isTerminal":true}`

	events := ompParseLines(t, sessionHdr, agentStart, unknown, turnStart, textEv, msgEnd, agentEnd)
	h, _ := newOmpReplayHandler(t)

	res := h.streamRun(context.Background(), "c1", "p1", ompEventChan(events), "spec-model", nil)
	if res.Err != nil {
		t.Fatalf("streamRun must not error on unknown events: %v", res.Err)
	}
	if !strings.Contains(res.Reply, "ok") {
		t.Errorf("reply lost terminal text: %q", res.Reply)
	}
}
