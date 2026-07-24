package renderer

import (
	"encoding/json"
	"fmt"
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
