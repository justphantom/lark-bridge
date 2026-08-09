package cardkit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// jsonOf marshals b and unmarshals into a generic map for assertion.
func jsonOf(t *testing.T, b []byte, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func TestCard(t *testing.T) {
	hdr := HeaderInfo{BackendType: "claude", Title: "标题", Template: "blue"}
	ftr := FooterInfo{BackendType: "claude", Model: "sonnet", Time: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)}
	b, err := Card(hdr, ftr, []Element{MarkdownElement("body")}, nil)
	card := jsonOf(t, b, err)

	header := card["header"].(map[string]any)
	title := header["title"].(map[string]any)
	if title["content"] != "[claude] 标题" {
		t.Errorf("header.title.content = %v, want [claude] 标题", title["content"])
	}
	if header["template"] != "blue" {
		t.Errorf("header.template = %v, want blue", header["template"])
	}
	// Root must only contain schema/config/header/elements — no root-level footer/actions.
	if _, ok := card["footer"]; ok {
		t.Error("footer must not be a root-level key")
	}
	if _, ok := card["actions"]; ok {
		t.Error("actions must not be a root-level key")
	}
	if _, ok := card["body"]; !ok {
		t.Error("body must be present in schema 2.0")
	}
	// config.update_multi 必须显式为 true：飞书 cardkit PUT 接口的硬性要求。
	cfg := card["config"].(map[string]any)
	if cfg["update_multi"] != true {
		t.Errorf("config.update_multi = %v, want true", cfg["update_multi"])
	}
	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	// 1 content element + 1 footer element = 2.
	if len(elements) != 2 {
		t.Fatalf("elements len = %d, want 2 (content + footer)", len(elements))
	}
}

func TestCardWithActions(t *testing.T) {
	hdr := HeaderInfo{BackendType: "x"}
	ftr := FooterInfo{BackendType: "x"}
	acts := []Action{
		ButtonAction("允许", "permission", map[string]any{"requestID": "r1"}, true, false),
		ButtonAction("拒绝", "permission", map[string]any{"requestID": "r1"}, false, false),
	}
	b, err := Card(hdr, ftr, []Element{MarkdownElement("m")}, acts)
	card := jsonOf(t, b, err)
	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	// schema 2.0：markdown + 1 column_set (button row) + footer = 3.
	if len(elements) != 3 {
		t.Fatalf("elements len = %d, want 3 (markdown + column_set + footer)", len(elements))
	}
	// schema 2.0：buttons ride a column_set > column > button layout.
	var buttons []map[string]any
	for _, el := range elements {
		elem, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if tag, _ := elem["tag"].(string); tag == "column_set" {
			cols, _ := elem["columns"].([]any)
			for _, c := range cols {
				col, _ := c.(map[string]any)
				ce, _ := col["elements"].([]any)
				for _, e := range ce {
					if btn, ok := e.(map[string]any); ok {
						buttons = append(buttons, btn)
					}
				}
			}
		}
	}
	if len(buttons) != 2 {
		t.Errorf("buttons len = %d, want 2 (inside column_set)", len(buttons))
	}
}

// TestFormSubmitButtonHasBehaviors pins the CardKit (schema 2.0) form
// requirement: a submit button must declare an explicit behaviors entry so
// Feishu routes the form values back as action.form_value. Without it the
// server rejects the card.
func TestFormSubmitButtonHasBehaviors(t *testing.T) {
	submit := SubmitButtonAction("提交", map[string]any{"requestID": "r"}, true)
	form := FormElement("f", []Element{Element(submit)})
	b, err := Card(HeaderInfo{Title: "t"}, FooterInfo{}, []Element{form}, nil)
	card := jsonOf(t, b, err)
	body := card["body"].(map[string]any)
	if _, has := body["elements"]; !has {
		t.Fatal("v2 card must use body.elements")
	}
	raw := string(b)
	if !strings.Contains(raw, "behaviors") {
		t.Fatalf("v2 submit button must contain behaviors: %s", raw)
	}
	elems := body["elements"].([]any)
	f := elems[0].(map[string]any)
	fe := f["elements"].([]any)
	last := fe[len(fe)-1].(map[string]any)
	if last["tag"] != "button" || last["action_type"] != "form_submit" {
		t.Fatalf("form submit button missing/mis-shaped: %v", last)
	}
}

func TestFooterContainsFields(t *testing.T) {
	ftr := FooterInfo{
		BackendType: "opencode",
		Model:       "gpt-4",
		SessionID:   "abcdef123456",
		Time:        time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC),
	}
	el := Footer(ftr)
	if el["tag"] != "div" {
		t.Errorf("footer tag = %v, want div", el["tag"])
	}
	text := el["text"].(map[string]any)
	content := text["content"].(string)
	for _, want := range []string{"opencode", "gpt-4", "2026-07-09 12:00:00", "abcdef12"} {
		if !strings.Contains(content, want) {
			t.Errorf("footer %q missing %q", content, want)
		}
	}
}

func TestFooterOmitsSessionWhenEmpty(t *testing.T) {
	ftr := FooterInfo{BackendType: "claude", Model: "m", Time: time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)}
	el := Footer(ftr)
	content := el["text"].(map[string]any)["content"].(string)
	// Only 3 parts when no session id: backendType · model · time.
	if c := strings.Count(content, " · "); c != 2 {
		t.Errorf("footer separator count = %d, want 2 (no session): %q", c, content)
	}
}

func TestHeaderRedTemplate(t *testing.T) {
	h := Header(HeaderInfo{BackendType: "x", Title: "err", Template: "red"})
	if h["template"] != "red" {
		t.Errorf("template = %v, want red", h["template"])
	}
}

func TestNoticeTemplates(t *testing.T) {
	cases := []struct {
		level, want string
	}{
		{"error", "red"},
		{"warning", "orange"},
		{"success", "green"},
		{"info", "grey"},
	}
	for _, c := range cases {
		b, err := Notice(FooterInfo{BackendType: "b"}, c.level, "t", "body", "", "", "")
		card := jsonOf(t, b, err)
		header := card["header"].(map[string]any)
		if header["template"] != c.want {
			t.Errorf("level %q template = %v, want %v", c.level, header["template"], c.want)
		}
	}
}

func TestButtonActionPrimary(t *testing.T) {
	a := ButtonAction("允许", "permission", map[string]any{"requestID": "r1"}, true, false)
	if a["type"] != "primary" {
		t.Errorf("type = %v, want primary", a["type"])
	}
	value := a["value"].(map[string]any)
	if value["kind"] != "permission" || value["requestID"] != "r1" {
		t.Errorf("value = %v", value)
	}
}

func TestButtonActionDisabled(t *testing.T) {
	a := ButtonAction("提交", "submit", nil, true, true)
	if a["disabled"] != true {
		t.Errorf("disabled = %v, want true", a["disabled"])
	}
}

// TestNoticeTruncatesLongBody verifies that a notice body longer than
// MaxBodyRunes is truncated so the card stays under Feishu's content limit.
func TestNoticeTruncatesLongBody(t *testing.T) {
	long := strings.Repeat("a", MaxBodyRunes*2)
	b, err := Notice(FooterInfo{BackendType: "b"}, "info", "t", long, "", "", "")
	if err != nil {
		t.Fatalf("Notice: %v", err)
	}
	if len(b) > 28*1024 {
		t.Errorf("card json = %d bytes, want <= %d", len(b), 28*1024)
	}
	if !strings.Contains(string(b), "…") {
		t.Error("expected truncation marker … in notice card")
	}
}

// TestNoticeWithChange verifies a setting-change notice renders the
// before→after block above the body when field/before/after are supplied.
func TestNoticeWithChange(t *testing.T) {
	b, err := Notice(FooterInfo{BackendType: "b"}, "success", "已设置权限", "下次提问生效。", "权限模式", "default", "plan")
	if err != nil {
		t.Fatalf("Notice: %v", err)
	}
	body := string(b)
	// Strikethrough old value + bold new value, field label present.
	for _, want := range []string{"权限模式", "~default~", "**plan**", "下次提问生效。"} {
		if !strings.Contains(body, want) {
			t.Errorf("change notice missing %q: %s", want, body)
		}
	}
}

// TestNoticeWithChangeNoBefore verifies a first-time set (empty before) omits
// the strikethrough half and just shows the arrow + new value.
func TestNoticeWithChangeNoBefore(t *testing.T) {
	b, err := Notice(FooterInfo{BackendType: "b"}, "success", "已设置", "", "模型", "", "sonnet")
	if err != nil {
		t.Fatalf("Notice: %v", err)
	}
	body := string(b)
	if strings.Contains(body, "~") {
		t.Errorf("first-time set should have no strikethrough: %s", body)
	}
	if !strings.Contains(body, "→ **sonnet**") {
		t.Errorf("expected arrow + bold new value: %s", body)
	}
}

// TestNoticeWithChangeTruncatesCombined verifies that when the change block and
// body together exceed MaxBodyRunes, the final markdown is capped at the limit
// and the change block (at the front) survives truncation.
func TestNoticeWithChangeTruncatesCombined(t *testing.T) {
	long := strings.Repeat("a", MaxBodyRunes)
	b, err := Notice(FooterInfo{BackendType: "b"}, "success", "已设置", long, "模型", "old", "sonnet")
	if err != nil {
		t.Fatalf("Notice: %v", err)
	}
	card := jsonOf(t, b, nil)
	body := card["body"].(map[string]any)
	elements := body["elements"].([]any)
	content := elements[0].(map[string]any)["content"].(string)
	if len([]rune(content)) > MaxBodyRunes+1 {
		t.Errorf("content runes = %d, want ≤ %d", len([]rune(content)), MaxBodyRunes+1)
	}
	if !strings.Contains(content, "模型") || !strings.Contains(content, "sonnet") {
		t.Errorf("change block should survive truncation: %s", content)
	}
	if !strings.Contains(content, "…") {
		t.Errorf("expected truncation marker … in combined content: %s", content)
	}
}

// TestCardRejectsTooManyElements verifies Card enforces Feishu's element
// hard cap so a pathological caller surfaces the failure as a Go error
// instead of a server-side 230025 rejection on every SendCard/UpdateCard.
func TestCardRejectsTooManyElements(t *testing.T) {
	hdr := HeaderInfo{BackendType: "x"}
	ftr := FooterInfo{BackendType: "x"}
	// Fill elements so elements+actions+footer just exceeds MaxCardElements.
	over := MaxCardElements - 1 // -1 for the footer
	elements := make([]Element, over)
	for i := range elements {
		elements[i] = MarkdownElement("x")
	}
	// Adding one action tips elements+actions+footer to MaxCardElements+1.
	acts := []Action{ButtonAction("ok", "permission", map[string]any{"requestID": "r"}, true, false)}
	if _, err := Card(hdr, ftr, elements, acts); err == nil {
		t.Fatalf("Card accepted %d elements (limit %d)", over+1+1, MaxCardElements)
	}

	// Sanity: just under the cap still builds.
	under := MaxCardElements - 2 // -1 footer, -1 action
	elems := make([]Element, under)
	for i := range elems {
		elems[i] = MarkdownElement("x")
	}
	if _, err := Card(hdr, ftr, elems, acts); err != nil {
		t.Fatalf("Card rejected %d elements (limit %d): %v", under+1+1, MaxCardElements, err)
	}
}
