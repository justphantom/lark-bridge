package renderer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

func hdr() cardkit.HeaderInfo { return cardkit.HeaderInfo{BackendType: "claude"} }
func ftr() cardkit.FooterInfo { return cardkit.FooterInfo{BackendType: "claude", Time: time.Now()} }

// parse unmarshals a rendered card (with its error asserted non-nil) into a
// generic map for assertions.
func parse(t *testing.T, b []byte, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

// firstMarkdownContent returns the content of the first markdown element in
// body.elements, failing the test if none exists. Used by tests that assert on
// real newline runes (JSON marshalling would otherwise escape them as "\n").
func firstMarkdownContent(t *testing.T, card map[string]any) string {
	t.Helper()
	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatal("missing body")
	}
	elements, _ := body["elements"].([]any)
	for _, el := range elements {
		em, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if em["tag"] == "markdown" {
			if s, ok := em["content"].(string); ok {
				return s
			}
		}
	}
	t.Fatal("no markdown element in body")
	return ""
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// actionButtons collects all {"tag":"button"} elements anywhere under
// body.elements, including buttons nested inside a form container. Returns
// nil when no buttons exist.
func actionButtons(t *testing.T, card map[string]any) []any {
	t.Helper()
	body, ok := card["body"].(map[string]any)
	if !ok {
		t.Fatal("missing body")
	}
	var buttons []any
	collectButtons(body, &buttons)
	return buttons
}

// collectButtons recursively walks node["elements"] collecting buttons,
// recursing into containers (form/column/...) that hold their own elements.
func collectButtons(node map[string]any, out *[]any) {
	elements, _ := node["elements"].([]any)
	for _, el := range elements {
		elem, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if tag, _ := elem["tag"].(string); tag == "button" {
			*out = append(*out, elem)
			continue
		}
		collectButtons(elem, out)
	}
}

func TestProgressRender(t *testing.T) {
	s := NewProgressState()
	s.AddToolUse("bash", "ls", false, "")
	s.AddToolResult("bash", "", "file.txt", false, false, "")
	s.AddProgress()
	b, err := s.Render(hdr(), ftr())
	card := parse(t, b, err)
	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	all := string(mustMarshal(t, elements))
	// "Bash" (name) and "ls" (desc) show; the completed tool's output
	// "file.txt" does NOT — the progress card shows actions, not output.
	for _, want := range []string{"Bash", "ls"} {
		if !strings.Contains(all, want) {
			t.Errorf("progress missing %q", want)
		}
	}
	if strings.Contains(all, "file.txt") {
		t.Errorf("completed tool output should not be shown: %s", all)
	}
	h := card["header"].(map[string]any)
	if h["template"] != "blue" {
		t.Errorf("template = %v, want blue", h["template"])
	}
}

func TestResultRender(t *testing.T) {
	ctrl := &protocol.Control{Result: &protocol.ResultPayload{Text: "done", Model: "sonnet", Tokens: 42, Duration: 5_000_000_000}}
	b, err := RenderResult(ctrl, hdr(), ftr(), "")
	card := parse(t, b, err)
	h := card["header"].(map[string]any)
	if h["template"] != "green" {
		t.Errorf("template = %v, want green", h["template"])
	}
	title := h["title"].(map[string]any)
	if !strings.Contains(title["content"].(string), "已完成") {
		t.Errorf("title = %v, want 已完成", title["content"])
	}
	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	md := string(mustMarshal(t, elements))
	if !strings.Contains(md, "done") || !strings.Contains(md, "42 tokens") {
		t.Errorf("result body missing text/stats: %s", md)
	}
}

// TestResultRender_TruncatesLongBody verifies that a result text longer than
// maxResultRunes is truncated to … so the card stays under Feishu's content
// limit.
func TestResultRender_TruncatesLongBody(t *testing.T) {
	long := strings.Repeat("a", maxResultRunes*2)
	ctrl := &protocol.Control{Result: &protocol.ResultPayload{Text: long}}
	b, err := RenderResult(ctrl, hdr(), ftr(), "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The output must stay well under Feishu's ~28 KiB card limit.
	if len(b) > 28*1024 {
		t.Errorf("card json = %d bytes, want <= %d", len(b), 28*1024)
	}
	if !strings.Contains(string(b), "…") {
		t.Error("expected truncation marker … in result card")
	}
}

// TestResultRender_WithSummary verifies the execution-summary line is rendered
// above the stats line when a non-empty summary is supplied (the dispatcher
// builds it from the progress state at turn end).
func TestResultRender_WithSummary(t *testing.T) {
	ctrl := &protocol.Control{Result: &protocol.ResultPayload{Text: "done", Tokens: 10}}
	b, err := RenderResult(ctrl, hdr(), ftr(), "📎 读取 77 · 执行 12 · 子代理 1")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	md := string(b)
	for _, want := range []string{"读取 77", "执行 12", "子代理 1", "10 tokens"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in result card: %s", want, md)
		}
	}
}

func TestQuestionRender(t *testing.T) {
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{RequestID: "r2", Questions: []protocol.QuestionItem{{Label: "pick", Options: []string{"a", "b"}, Multiple: true, Custom: true}}}}
	b, err := RenderQuestion(ctrl, hdr(), ftr())
	card := parse(t, b, err)
	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	all := string(mustMarshal(t, elements))
	if !strings.Contains(all, "multi_select_static") {
		t.Errorf("question missing multi-select: %s", all)
	}
	if !strings.Contains(all, "custom") {
		t.Errorf("question missing custom input: %s", all)
	}
	actions := actionButtons(t, card)
	if len(actions) != 1 {
		t.Fatalf("actions = %d, want 1 submit", len(actions))
	}
}

// TestRenderPermission_StructuredBody pins the typed form: when Title is set,
// the renderer uses Type/Title/Detail (bold badge + headline + code-block
// detail) and ignores the flat Message.
func TestRenderPermission_StructuredBody(t *testing.T) {
	ctrl := &protocol.Control{Permission: &protocol.PermissionPayload{
		RequestID: "p1",
		Message:   "请求执行 bash", // flat fallback, must be ignored when Title set
		Type:      "Bash",
		Title:     "make test",
		Detail:    "make test\nnpm run build",
		Options:   []protocol.PermissionOption{{Label: "允许", Value: "allow"}},
	}}
	b, err := RenderPermission(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	// Pull the first markdown element's content so newline assertions work on
	// real runes, not JSON's escaped "\n".
	card := parse(t, b, nil)
	content := firstMarkdownContent(t, card)
	if !strings.Contains(content, "**Bash**") {
		t.Errorf("type badge missing: %s", content)
	}
	if !strings.Contains(content, "make test") {
		t.Errorf("title headline missing: %s", content)
	}
	if !strings.Contains(content, "```\nmake test\nnpm run build\n```") {
		t.Errorf("detail code block missing: %s", content)
	}
	// Flat Message must NOT appear once the structured form is used.
	if strings.Contains(content, "请求执行 bash") {
		t.Errorf("flat message should be dropped when Title is set: %s", content)
	}
}

// TestRenderPermission_StructuredDetailDroppedWhenEqualsTitle ensures the
// detail block is omitted when Detail equals Title, so a single-pattern
// permission (Title==Detail) does not echo the headline in a code block.
func TestRenderPermission_StructuredDetailDroppedWhenEqualsTitle(t *testing.T) {
	ctrl := &protocol.Control{Permission: &protocol.PermissionPayload{
		RequestID: "p1",
		Type:      "Read",
		Title:     "/etc/hosts",
		Detail:    "/etc/hosts",
		Options:   []protocol.PermissionOption{{Label: "允许", Value: "allow"}},
	}}
	b, err := RenderPermission(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	card := parse(t, b, nil)
	if strings.Contains(firstMarkdownContent(t, card), "```") {
		t.Errorf("no code block expected when Detail==Title: %s", b)
	}
}

// TestRenderInteractive_QuestionAsButtons verifies a small single-select
// question (one question, ≤4 options, no custom) renders as immediate-click
// buttons (kind=question) instead of a dropdown+submit form, so the user
// answers with one click.
func TestRenderInteractive_QuestionAsButtons(t *testing.T) {
	ctrl := &protocol.Control{Type: protocol.TypeQuestion, Question: &protocol.QuestionPayload{
		RequestID: "q1",
		Questions: []protocol.QuestionItem{{
			Label:   "选模型",
			Options: []string{"a", "b", "c"},
		}},
	}}
	b, err := RenderInteractive(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	card := parse(t, b, nil)
	buttons := actionButtons(t, card)
	if len(buttons) != 3 {
		t.Fatalf("want 3 immediate-click buttons, got %d", len(buttons))
	}
	all := string(mustMarshal(t, buttons))
	if strings.Contains(all, "select_static") || strings.Contains(all, "form_submit") {
		t.Errorf("small question should not use dropdown/form: %s", all)
	}
	if !strings.Contains(all, `"kind":"question"`) {
		t.Errorf("button kind=question missing: %s", all)
	}
	// Each option label round-trips as the choice value (label-as-value, same
	// convention as the dropdown path).
	for _, opt := range []string{"a", "b", "c"} {
		if !strings.Contains(all, `"choice":"`+opt+`"`) {
			t.Errorf("choice %q missing: %s", opt, all)
		}
	}
}

// TestRenderInteractive_QuestionStillFormWhenLarge verifies a question that
// does NOT fit the button profile (many options, multi-select, custom, or
// multi-question) still renders via the dropdown+submit form.
func TestRenderInteractive_QuestionStillFormWhenLarge(t *testing.T) {
	cases := []struct {
		name string
		q    *protocol.QuestionPayload
	}{
		{"many options", &protocol.QuestionPayload{RequestID: "q", Questions: []protocol.QuestionItem{{Label: "x", Options: []string{"a", "b", "c", "d", "e"}}}}},
		{"multi-select", &protocol.QuestionPayload{RequestID: "q", Questions: []protocol.QuestionItem{{Label: "x", Multiple: true, Options: []string{"a", "b"}}}}},
		{"custom input", &protocol.QuestionPayload{RequestID: "q", Questions: []protocol.QuestionItem{{Label: "x", Custom: true, Options: []string{"a", "b"}}}}},
		{"multi-question", &protocol.QuestionPayload{RequestID: "q", Questions: []protocol.QuestionItem{{Label: "x", Options: []string{"a"}}, {Label: "y", Options: []string{"b"}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := RenderInteractive(&protocol.Control{Type: protocol.TypeQuestion, Question: tc.q}, hdr(), ftr())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), "select_static") {
				t.Errorf("large/multi/custom question should use the dropdown form: %s", b)
			}
		})
	}
}

func TestPermissionRender(t *testing.T) {
	ctrl := &protocol.Control{Permission: &protocol.PermissionPayload{
		RequestID: "p1",
		Message:   "请求执行 bash",
		Options: []protocol.PermissionOption{
			{Label: "允许", Value: "allow"},
			{Label: "拒绝", Value: "deny"},
		},
	}}
	b, err := RenderPermission(ctrl, hdr(), ftr())
	card := parse(t, b, err)
	actions := actionButtons(t, card)
	if len(actions) != 2 {
		t.Fatalf("actions = %d, want 2 buttons", len(actions))
	}
	all := string(mustMarshal(t, actions))
	if strings.Contains(all, "select_static") {
		t.Errorf("permission card must not use a dropdown: %s", all)
	}
	if !strings.Contains(all, `"kind":"permission"`) {
		t.Errorf("button kind=permission missing: %s", all)
	}
	if !strings.Contains(all, `"choice":"allow"`) || !strings.Contains(all, `"choice":"deny"`) {
		t.Errorf("choice values missing: %s", all)
	}
	if !strings.Contains(string(b), "请求执行 bash") {
		t.Errorf("message body missing: %s", b)
	}
}

// TestRenderInteractive_DispatchesPermission verifies RenderInteractive routes
// a TypePermission control to the button renderer (not the question dropdown).
func TestRenderInteractive_DispatchesPermission(t *testing.T) {
	ctrl := &protocol.Control{Type: protocol.TypePermission, Permission: &protocol.PermissionPayload{
		RequestID: "p1", Options: []protocol.PermissionOption{{Label: "a", Value: "a"}},
	}}
	b, err := RenderInteractive(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"kind":"permission"`) {
		t.Errorf("TypePermission should render buttons: %s", b)
	}
}

// TestRenderQuestion_TrimsTooManyQuestions verifies a Question payload with
// more than maxQuestions items is truncated to the cap and the omitted-count
// is surfaced to the user, instead of producing a card Feishu would reject
// server-side (230025) for exceeding the element hard limit.
func TestRenderQuestion_TrimsTooManyQuestions(t *testing.T) {
	items := make([]protocol.QuestionItem, maxQuestions+5)
	for i := range items {
		items[i] = protocol.QuestionItem{Label: "q" + strconv.Itoa(i), Options: []string{"a"}}
	}
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{RequestID: "r", Questions: items}}
	b, err := RenderQuestion(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	all := string(b)
	// The first kept label survives; the cap+1'th label is dropped.
	if !strings.Contains(all, "q0") {
		t.Errorf("first question should survive: %s", all)
	}
	if strings.Contains(all, "q"+strconv.Itoa(maxQuestions)) {
		t.Errorf("question past cap should be dropped: %s", all)
	}
	if !strings.Contains(all, "已省略 5") {
		t.Errorf("omitted count notice missing: %s", all)
	}
}

// TestRenderPermission_TrimsTooManyOptions verifies a Permission payload with
// more than maxPermissionOptions is truncated so the resulting card stays
// under cardkit.MaxCardElements (no Card error, no server-side rejection).
func TestRenderPermission_TrimsTooManyOptions(t *testing.T) {
	opts := make([]protocol.PermissionOption, maxPermissionOptions+10)
	for i := range opts {
		opts[i] = protocol.PermissionOption{Label: "o" + strconv.Itoa(i), Value: "v" + strconv.Itoa(i)}
	}
	ctrl := &protocol.Control{Permission: &protocol.PermissionPayload{RequestID: "r", Options: opts}}
	b, err := RenderPermission(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatalf("render %d options: %v", len(opts), err)
	}
	card := parse(t, b, nil)
	actions := actionButtons(t, card)
	// Permission has no separate submit button (each option submits
	// immediately), so buttons == maxPermissionOptions == 47. Total card
	// elements (msg + wait notice + footer + 47 buttons) == MaxCardElements.
	if len(actions) != maxPermissionOptions {
		t.Errorf("buttons = %d, want %d (capped options, no submit)", len(actions), maxPermissionOptions)
	}
}

func TestInteractiveSubmitted(t *testing.T) {
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{RequestID: "r1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}}}
	orig, err := RenderQuestion(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	sb, serr := RenderInteractiveSubmitted(orig, "✓ 你选择了「允许」")
	submitted := parse(t, sb, serr)
	actions := actionButtons(t, submitted)
	for _, a := range actions {
		btn := a.(map[string]any)
		if btn["disabled"] != true {
			t.Errorf("button not disabled: %v", btn)
		}
	}
	all := string(mustMarshal(t, actions))
	if !strings.Contains(all, "已提交") {
		t.Errorf("submitted primary label missing: %s", all)
	}
	if !strings.Contains(string(sb), "你选择了「允许」") {
		t.Errorf("summary echo missing: %s", sb)
	}
}

// TestInteractiveSubmitted_FlipsFooterStatus verifies the footer status word
// advances from "待确认" to "处理中" once the user submits, so the card reads
// as past the pending state (design scheme ③ state-2 requirement).
func TestInteractiveSubmitted_FlipsFooterStatus(t *testing.T) {
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{RequestID: "r1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}}}
	footer := cardkit.FooterInfo{BackendType: "opencode", Status: "待确认", SessionID: "abcdef123456"}
	orig, err := RenderQuestion(ctrl, hdr(), footer)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := RenderInteractiveSubmitted(orig, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sb), "处理中 · opencode") {
		t.Errorf("footer should flip to 处理中: %s", sb)
	}
	if strings.Contains(string(sb), "待确认") {
		t.Errorf("footer should not still show 待确认: %s", sb)
	}
}

// TestPermissionRender_TruncatesLongBody verifies that a permission message
// longer than the body budget is truncated so the card stays under Feishu's
// TestQuestionRender_TruncatesLongOptions verifies that a question whose
// options collectively exceed the body budget has later options dropped so the
// card stays under Feishu's content limit.
func TestQuestionRender_TruncatesLongOptions(t *testing.T) {
	// Many long options whose concatenated text far exceeds MaxBodyRunes.
	opts := make([]string, 500)
	for i := range opts {
		opts[i] = strings.Repeat("x", 50)
	}
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{
		RequestID: "r2",
		Questions: []protocol.QuestionItem{{Label: "pick", Options: opts}},
	}}
	b, err := RenderQuestion(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if len(b) > 28*1024 {
		t.Errorf("card json = %d bytes, want <= %d", len(b), 28*1024)
	}
	if !strings.Contains(string(b), "已省略") {
		t.Error("expected options-omitted marker in question card")
	}
}

// TestQuestionRender_OptionBudgetFitsLargeList verifies the raised option
// rune budget (maxQuestionOptionRunes) accommodates a realistic large picker
// list that would have been truncated under the old 4000-rune body budget.
// 100 options of 15 runes each = 100*(15+40) = 5500 runes: under 15000, over
// the old 4000.
func TestQuestionRender_OptionBudgetFitsLargeList(t *testing.T) {
	opts := make([]string, 100)
	for i := range opts {
		opts[i] = "provider/model-" + strings.Repeat("x", 5) // ~15 runes each
	}
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{
		RequestID: "r3",
		Questions: []protocol.QuestionItem{{Label: "pick", Options: opts}},
	}}
	b, err := RenderQuestion(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(string(b), "已省略") {
		t.Error("100 short options (~5500 runes) should fit in the 15000 budget, no truncation expected")
	}
}

// TestRenderInteractiveExpired verifies the expired form carries the failure
// notice and disables buttons.
func TestRenderInteractiveExpired(t *testing.T) {
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{RequestID: "r1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}}}
	orig, err := RenderQuestion(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderInteractiveExpired(orig)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if !strings.Contains(body, "已自动失效") {
		t.Errorf("expiry notice missing: %s", body)
	}
	for _, btn := range actionButtons(t, parse(t, b, nil)) {
		if btn.(map[string]any)["disabled"] != true {
			t.Errorf("button not disabled on expired card: %v", btn)
		}
	}
}

// TestInteractiveTimeoutHintFromConstant verifies the pending/expired hints use
// the minutes from cardkit.InteractiveTimeout instead of a hardcoded value.
func TestInteractiveTimeoutHintFromConstant(t *testing.T) {
	ctrl := &protocol.Control{Question: &protocol.QuestionPayload{RequestID: "r1", Questions: []protocol.QuestionItem{{Label: "q", Options: []string{"a"}}}}}
	b, err := RenderQuestion(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d 分钟后自动失效", int(cardkit.InteractiveTimeout.Minutes()))
	if !strings.Contains(string(b), want) {
		t.Errorf("permission hint missing %q: %s", want, b)
	}

	exp, err := RenderInteractiveExpired(b)
	if err != nil {
		t.Fatal(err)
	}
	wantExpired := fmt.Sprintf("%d 分钟未响应", int(cardkit.InteractiveTimeout.Minutes()))
	if !strings.Contains(string(exp), wantExpired) {
		t.Errorf("expired notice missing %q: %s", wantExpired, exp)
	}
}

// TestRenderInteractiveFinalized_KeepsConfirm pins C5: when a card already
// shows a submit confirmation ("✓ …"), Finalized must NOT clobber it with the
// generic "结果见上方" line — the user's choice echo is the more useful story.
// Only the footer status flips to 已完成.
func TestRenderInteractiveFinalized_KeepsConfirm(t *testing.T) {
	ctrl := &protocol.Control{Type: protocol.TypePermission, Permission: &protocol.PermissionPayload{
		RequestID: "p1", Options: []protocol.PermissionOption{{Label: "允许", Value: "allow"}},
	}}
	orig, err := RenderInteractive(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := RenderInteractiveSubmitted(orig, "✓ 你选择了「允许」")
	if err != nil {
		t.Fatal(err)
	}
	fin, err := RenderInteractiveFinalized(submitted)
	if err != nil {
		t.Fatal(err)
	}
	card := parse(t, fin, nil)
	content := firstMarkdownContent(t, card)
	if !strings.HasPrefix(content, "✓ 你选择了「允许」") {
		t.Errorf("finalize clobbered the submit confirmation; first markdown = %q", content)
	}
	if strings.Contains(content, "结果见上方") {
		t.Errorf("generic pointer should be suppressed when ✓ confirmation exists: %s", content)
	}
}

// TestRenderInteractiveFinalized_AddsPointerWhenNoConfirm pins the other half:
// a card the turn ended without the user acting on gets the generic pointer,
// since there is no choice echo to keep.
func TestRenderInteractiveFinalized_AddsPointerWhenNoConfirm(t *testing.T) {
	ctrl := &protocol.Control{Type: protocol.TypePermission, Permission: &protocol.PermissionPayload{
		RequestID: "p1", Message: "请求执行 bash", Options: []protocol.PermissionOption{{Label: "允许", Value: "allow"}},
	}}
	orig, err := RenderInteractive(ctrl, hdr(), ftr())
	if err != nil {
		t.Fatal(err)
	}
	fin, err := RenderInteractiveFinalized(orig)
	if err != nil {
		t.Fatal(err)
	}
	card := parse(t, fin, nil)
	content := firstMarkdownContent(t, card)
	if !strings.HasPrefix(content, "✓ 本轮已完成，结果见上方卡片。") {
		t.Errorf("generic pointer should be prepended when no ✓ confirmation: %q", content)
	}
}
