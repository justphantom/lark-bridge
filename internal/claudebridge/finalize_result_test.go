package claudebridge

import (
	"testing"

	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/router"
)

// TestFinalizeResult_ReplySource locks in the multi-turn reply fix: the
// reply must come from the last assistant message's accumulated text
// (lastReply), not the result envelope's "result" field. In multi-turn
// runs (subagents, tool rounds) Claude Code's "result" field can hold an
// early turn's text rather than the final answer, so lastReply is the
// source of truth and result.result is only a fallback when no assistant
// text arrived (e.g. an error result with no preceding assistant text).
func TestFinalizeResult_ReplySource(t *testing.T) {
	client, _, cleanup := connectTestRPC(t)
	defer cleanup()
	r, _ := router.New("", log.Nop())
	h := NewWithLogger(r, &scriptClaude{}, client, HandlerConfig{
		StateDir: t.TempDir(),
	}, log.Nop())

	cases := []struct {
		name      string
		result    string // result envelope's "result" field (turn-1 in the bug)
		lastReply string // last assistant message's accumulated text
		want      string
	}{
		{
			// Reproduces the captured jsonl: result.result held turn-1's
			// "已为根目录下 6 个文档..." (the agent-dispatch notice) while the
			// real final answer was the last turn's summary.
			name:      "multi-turn: last assistant text wins over result.result",
			result:    "已为根目录下 6 个文档各指派一个 agent 并行审查正确性",
			lastReply: "全部 6 个文档审查完成。汇总如下。",
			want:      "全部 6 个文档审查完成。汇总如下。",
		},
		{
			// No assistant text reached the accumulator (e.g. an error before
			// any reply): the result field is the only signal, so fall back.
			name:      "no assistant text: fall back to result.result",
			result:    "fallback text from result envelope",
			lastReply: "",
			want:      "fallback text from result envelope",
		},
		{
			// Single-turn: both sources agree; lastReply is still preferred.
			name:      "single-turn: lastReply equals result",
			result:    "done",
			lastReply: "done",
			want:      "done",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := claude.Event{
				Type:     claude.EventResult,
				Subtype:  "success",
				IsError:  false,
				Result:   tc.result,
				NumTurns: 8,
			}
			got := h.finalizeResult(ev, tc.lastReply, "s1", "glm-5.2", "", "c1")

			if got.reply != tc.want {
				t.Errorf("reply = %q, want %q", got.reply, tc.want)
			}
			// Regression guard for the multi-turn case: when a distinct
			// lastReply exists, the reply must NOT be the early-turn
			// result.result.
			if tc.lastReply != "" && tc.result != tc.lastReply && got.reply == tc.result {
				t.Errorf("reply matched result.result %q; should prefer lastReply", tc.result)
			}
			// Result-event metadata is still parsed regardless of reply source.
			if got.steps != 8 {
				t.Errorf("steps = %d, want 8 (num_turns from result event)", got.steps)
			}
		})
	}
}
