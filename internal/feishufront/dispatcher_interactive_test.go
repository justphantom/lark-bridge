package feishufront

import (
	"testing"

	"github.com/justphantom/lark-bridge/internal/feishu"
)

// TestSubmitSummary_PermissionChoice covers the permission-card branch:
// value["choice"] drives choiceLabel. The allow/deny pair predates this test;
// confirm/cancel pin the G1 fix (opencode /session-clean confirmation cards
// previously echoed the raw English value).
func TestSubmitSummary_PermissionChoice(t *testing.T) {
	cases := map[string]string{
		"allow":   "✓ 你选择了「允许」",
		"deny":    "✓ 你选择了「拒绝」",
		"confirm": "✓ 你选择了「确认」",
		"cancel":  "✓ 你选择了「取消」",
	}
	for choice, want := range cases {
		action := &feishu.CardAction{Value: map[string]any{"choice": choice}}
		if got := submitSummary(action); got != want {
			t.Errorf("choice=%q: got %q, want %q", choice, got, want)
		}
	}
}

// TestSubmitSummary_UnknownChoicePassthrough verifies an unmapped choice value
// is returned verbatim (the default branch) rather than swallowed, so a future
// card type adding a new value is observable instead of silently blank.
func TestSubmitSummary_UnknownChoicePassthrough(t *testing.T) {
	action := &feishu.CardAction{Value: map[string]any{"choice": "authorize"}}
	if got := submitSummary(action); got != "✓ 你选择了「authorize」" {
		t.Errorf("got %q, want passthrough of raw value", got)
	}
}

// TestSubmitSummary_QuestionForm covers the question-card branch: form_value
// is parsed into choices + custom, and custom wins over a listed selection
// (the user explicitly overrode the list).
func TestSubmitSummary_QuestionForm(t *testing.T) {
	// A single listed selection.
	action := &feishu.CardAction{FormValue: map[string]any{"q_0": "glm-5.2"}}
	if got := submitSummary(action); got != "✓ 已回答: glm-5.2" {
		t.Errorf("listed select: got %q, want %q", got, "✓ 已回答: glm-5.2")
	}

	// Custom input overrides the listed selection.
	action = &feishu.CardAction{FormValue: map[string]any{
		"q_0":      "claude-haiku",
		"custom_0": "glm-5.2-custom",
	}}
	if got := submitSummary(action); got != "✓ 已回答: glm-5.2-custom" {
		t.Errorf("custom override: got %q, want %q", got, "✓ 已回答: glm-5.2-custom")
	}

	// Multi-select yields a "、"-joined summary.
	action = &feishu.CardAction{FormValue: map[string]any{"q_0": []any{"read", "write"}}}
	if got := submitSummary(action); got != "✓ 已回答: read、write" {
		t.Errorf("multi-select: got %q, want %q", got, "✓ 已回答: read、write")
	}
}

// TestSubmitSummary_EmptySubmission covers the fallthrough: no choice and no
// (parseable) form value yields the generic "已提交" banner.
func TestSubmitSummary_EmptySubmission(t *testing.T) {
	want := "✓ 已提交，正在处理…"
	if got := submitSummary(&feishu.CardAction{}); got != want {
		t.Errorf("empty action: got %q, want %q", got, want)
	}
	// form_value present but every selection blank → parseQuestionFormValue
	// returns nothing usable → same fallthrough.
	if got := submitSummary(&feishu.CardAction{FormValue: map[string]any{"q_0": ""}}); got != want {
		t.Errorf("blank form: got %q, want %q", got, want)
	}
}

// TestChoiceLabel pins the full mapping table in one place, including the
// G1 confirm/cancel additions, so a regression to the pre-G1 two-case switch
// is caught directly.
func TestChoiceLabel(t *testing.T) {
	cases := map[string]string{
		"allow":   "允许",
		"deny":    "拒绝",
		"confirm": "确认",
		"cancel":  "取消",
		"approve": "approve", // unknown → passthrough
		"":        "",
	}
	for in, want := range cases {
		if got := choiceLabel(in); got != want {
			t.Errorf("choiceLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestQuestionAnswerSummary covers the precedence and join rules: custom wins
// over choices; choices join with "、"; both empty → "" (which submitSummary
// turns into the generic banner).
func TestQuestionAnswerSummary(t *testing.T) {
	if got := questionAnswerSummary([]string{"a", "b"}, "custom"); got != "custom" {
		t.Errorf("custom should win: got %q", got)
	}
	if got := questionAnswerSummary([]string{"a", "b"}, ""); got != "a、b" {
		t.Errorf("join: got %q, want a、b", got)
	}
	if got := questionAnswerSummary([]string{"only"}, ""); got != "only" {
		t.Errorf("single: got %q, want only", got)
	}
	if got := questionAnswerSummary(nil, ""); got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}
}
