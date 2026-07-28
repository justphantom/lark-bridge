package feishu

import (
	"strings"
	"testing"
)

// TestParsePost_EmptyContent verifies empty / whitespace-only content
// returns an empty Post (with non-nil Blocks) and no error — the dispatcher
// must never nil-deref Blocks when ranging.
func TestParsePost_EmptyContent(t *testing.T) {
	cases := []string{"", "   ", "\n\n"}
	for _, c := range cases {
		p, err := ParsePost(c)
		if err != nil {
			t.Fatalf("ParsePost(%q): %v", c, err)
		}
		if p == nil {
			t.Fatalf("ParsePost(%q): nil post", c)
		}
		if p.Blocks == nil {
			t.Errorf("ParsePost(%q): Blocks is nil; want non-nil empty", c)
		}
	}
}

// TestParsePost_StandardLayout verifies the canonical {locale: {title,
// content}} shape parses into the expected AST. Uses zh_cn (Feishu domestic
// default) — locale key choice should not matter per the design contract.
func TestParsePost_StandardLayout(t *testing.T) {
	content := `{
		"zh_cn": {
			"title": "周报",
			"content": [
				[{"tag":"text","text":"本周完成 "},{"tag":"a","text":"报告","href":"https://x/y"}],
				[{"tag":"at","user_id":"ou_1","user_name":"张三"},{"tag":"text","text":" 请审阅"}]
			]
		}
	}`
	p, err := ParsePost(content)
	if err != nil {
		t.Fatalf("ParsePost: %v", err)
	}
	if p.Title != "周报" {
		t.Errorf("Title = %q, want 周报", p.Title)
	}
	if len(p.Blocks) != 2 {
		t.Fatalf("Blocks len = %d, want 2", len(p.Blocks))
	}
	if got := p.Blocks[0][0].Text; got != "本周完成 " {
		t.Errorf("first node text = %q", got)
	}
	a := p.Blocks[0][1]
	if a.Tag != "a" || a.Text != "报告" || a.Href != "https://x/y" {
		t.Errorf("a node = %+v", a)
	}
	at := p.Blocks[1][0]
	if at.Tag != "at" || at.UserID != "ou_1" || at.UserName != "张三" {
		t.Errorf("at node = %+v", at)
	}
}

// TestParsePost_LocaleFallback verifies any locale key works: en_us
// (Larksuite), zh_cn (Feishu), and any custom key all resolve to the same
// AST because post messages always carry exactly one locale block.
func TestParsePost_LocaleFallback(t *testing.T) {
	for _, locale := range []string{"zh_cn", "en_us", "ja_jp", "custom_x"} {
		content := `{"` + locale + `":{"content":[[{"tag":"text","text":"hi"}]]}}`
		p, err := ParsePost(content)
		if err != nil {
			t.Fatalf("locale %q: %v", locale, err)
		}
		if len(p.Blocks) != 1 || p.Blocks[0][0].Text != "hi" {
			t.Errorf("locale %q: got blocks %+v", locale, p.Blocks)
		}
	}
}

// TestParsePost_CorruptJSON verifies malformed JSON returns an error rather
// than panicking or producing a partial AST. The dispatcher must surface a
// "无法解析富文本" notice on this path.
func TestParsePost_CorruptJSON(t *testing.T) {
	cases := []string{
		"not json",
		`{"zh_cn":`,
		`{"zh_cn":{"content":"should-be-array"}}`,
	}
	for _, c := range cases {
		if _, err := ParsePost(c); err == nil {
			t.Errorf("ParsePost(%q): want error, got nil", c)
		}
	}
}

// TestParsePost_NoContent verifies a locale block with the content field
// absent yields an empty (non-nil) Blocks slice, not nil — same contract as
// the empty-content case.
func TestParsePost_NoContent(t *testing.T) {
	p, err := ParsePost(`{"zh_cn":{"title":"仅标题"}}`)
	if err != nil {
		t.Fatalf("ParsePost: %v", err)
	}
	if p.Title != "仅标题" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.Blocks == nil || len(p.Blocks) != 0 {
		t.Errorf("Blocks = %+v, want empty non-nil", p.Blocks)
	}
}

// TestParsePost_FlatLayout verifies the flat wire shape newer Feishu clients
// send: no locale wrapper, content sits at the top level alongside title
// (and content_v2, which must be ignored). A top-level "content" key is what
// tells ParsePost this is flat rather than locale-wrapped. Reproduces the
// real payload observed in production (group chat post with content_v2).
func TestParsePost_FlatLayout(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantTitle string
		wantRows  int
		wantText  string // first row, second node
	}{
		{
			name: "title and content_v2 present",
			content: `{
				"title": "",
				"content": [
					[{"tag":"text","text":"1. ","style":[]},{"tag":"text","text":"修 flaky 测试","style":[]}],
					[{"tag":"text","text":"2. ","style":[]},{"tag":"a","text":"CHANGELOG","href":"https://x/y","style":[]}]
				],
				"content_v2": [[{"tag":"text","text":"v2-must-not-leak"}]]
			}`,
			wantTitle: "",
			wantRows:  2,
			wantText:  "修 flaky 测试",
		},
		{
			name:      "content only no title no v2",
			content:   `{"content":[[{"tag":"text","text":"- "},{"tag":"text","text":"only"}]]}`,
			wantTitle: "",
			wantRows:  1,
			wantText:  "only",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := ParsePost(c.content)
			if err != nil {
				t.Fatalf("ParsePost: %v", err)
			}
			if p.Title != c.wantTitle {
				t.Errorf("Title = %q, want %q", p.Title, c.wantTitle)
			}
			if len(p.Blocks) != c.wantRows {
				t.Fatalf("Blocks len = %d, want %d", len(p.Blocks), c.wantRows)
			}
			if got := p.Blocks[0][1].Text; got != c.wantText {
				t.Errorf("first row second node text = %q, want %q", got, c.wantText)
			}
			for _, row := range p.Blocks {
				for _, n := range row {
					if n.Text == "v2-must-not-leak" {
						t.Errorf("content_v2 leaked into AST: %+v", n)
					}
				}
			}
		})
	}
}

// TestRenderNode covers each supported tag's renderer branch, including
// the "image_key / file_key not materialised" contract for img and media.
func TestRenderNode(t *testing.T) {
	cases := []struct {
		name string
		node PostNode
		want string
	}{
		{"text", PostNode{Tag: "text", Text: "hello"}, "hello"},
		{"a full", PostNode{Tag: "a", Text: "link", Href: "https://x"}, "[link](https://x)"},
		{"a no text", PostNode{Tag: "a", Href: "https://x"}, "https://x"},
		{"a no href", PostNode{Tag: "a", Text: "link"}, "link"},
		{"at with name", PostNode{Tag: "at", UserName: "张三"}, "@张三"},
		{"at no name", PostNode{Tag: "at", UserID: "ou_x"}, "@用户"},
		{"emotion", PostNode{Tag: "emotion", Emoji: "😀"}, "😀"},
		{"code_block no lang", PostNode{Tag: "code_block", Text: "x = 1"}, "```\nx = 1\n```"},
		{"code_block with lang", PostNode{Tag: "code_block", Language: "go", Text: "var x = 1"}, "```go\nvar x = 1\n```"},
		{"img returns empty", PostNode{Tag: "img", ImageKey: "img_v3_x"}, ""},
		{"media returns empty", PostNode{Tag: "media", FileKey: "file_v2_x"}, ""},
		{"unknown tag", PostNode{Tag: "future_tag", Text: "ignored"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := RenderNodeToMarkdown(c.node); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRenderPostToMarkdown exercises the full-AST renderer: title heading,
// paragraph separation, and img/media placeholder substitution. This is the
// path used when the file pipeline is disabled (no image download); the
// enabled path walks Blocks itself.
func TestRenderPostToMarkdown(t *testing.T) {
	p := &Post{
		Title: "标题",
		Blocks: [][]PostNode{
			{
				{Tag: "text", Text: "看 "},
				{Tag: "a", Text: "链接", Href: "https://x"},
				{Tag: "text", Text: " 截图："},
				{Tag: "img", ImageKey: "img_v3_1"},
			},
			{
				{Tag: "text", Text: "视频："},
				{Tag: "media", FileKey: "file_v2_1"},
			},
		},
	}
	got := RenderPostToMarkdown(p)
	wantSubstrings := []string{
		"# 标题",
		"看 [链接](https://x) 截图：",
		"[图片]",
		"视频：",
		"[视频]",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in:\n%s", s, got)
		}
	}
	// Two paragraphs separated by a blank line.
	if !strings.Contains(got, "\n\n视频：") {
		t.Errorf("paragraph separation missing:\n%s", got)
	}
}

// TestRenderPostToMarkdown_NilSafe verifies the renderer never panics on a
// nil Post (defensive: dispatcher may reach it on a malformed path).
func TestRenderPostToMarkdown_NilSafe(t *testing.T) {
	if got := RenderPostToMarkdown(nil); got != "" {
		t.Errorf("nil post renders %q, want empty", got)
	}
}
