package feishu

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PostNode is one inline element of a Feishu post-type message. The field
// set is the union of all tag variants; only the fields relevant to Tag are
// populated. Keeping it a flat struct (vs. a sum type with one struct per
// tag) keeps the JSON unmarshal simple and the renderer's switch tight.
//
// Wire format: each node is a JSON object whose "tag" key selects the
// variant. Unknown tags survive parsing (Tag carries the literal name) and
// render to "" so an unsupported tag never crashes the dispatcher.
type PostNode struct {
	Tag      string `json:"tag"`
	Text     string `json:"text,omitempty"`      // text / a
	Href     string `json:"href,omitempty"`      // a
	UserID   string `json:"user_id,omitempty"`   // at
	UserName string `json:"user_name,omitempty"` // at
	ImageKey string `json:"image_key,omitempty"` // img
	FileKey  string `json:"file_key,omitempty"`  // media
	Emoji    string `json:"emoji,omitempty"`     // emotion
	Language string `json:"language,omitempty"`  // code_block (rare)
}

// Post is the parsed AST of a Feishu post-type message.
//
// Blocks is a two-dimensional slice mirroring the wire format: the outer
// dimension is the paragraph (each paragraph renders separated by a blank
// line in Markdown), the inner dimension is the inline node sequence. A
// post with empty content unmarshals to an empty (non-nil) Blocks slice,
// never to nil, so callers can range over it without a guard.
type Post struct {
	Title  string
	Blocks [][]PostNode
}

// ParsePost decodes a post-type message's Content field into a Post AST.
//
// Locale resolution: a post always carries exactly one locale block; the
// key name (zh_cn / en_us / ...) is informational. We unmarshal into a
// map[string]*json.RawMessage and take the first value rather than maintain
// a locale priority list, which would always be arbitrary.
//
// Returns an error only on JSON corruption (the wire format changed, or
// Feishu sent malformed content). Missing optional fields (title, empty
// blocks) do not error: an empty post returns &Post{} + nil.
func ParsePost(content string) (*Post, error) {
	if strings.TrimSpace(content) == "" {
		return &Post{Blocks: [][]PostNode{}}, nil
	}
	// Outer wrap: { "<locale>": { "title": ..., "content": [...] } }.
	var locales map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &locales); err != nil {
		return nil, fmt.Errorf("feishu: parse post locales: %w", err)
	}
	if len(locales) == 0 {
		return &Post{Blocks: [][]PostNode{}}, nil
	}
	// Take the first locale block. Map iteration order is unspecified but
	// Feishu always sends exactly one; picking any is correct.
	var raw json.RawMessage
	for _, v := range locales {
		raw = v
		break
	}
	var block struct {
		Title   string       `json:"title"`
		Content [][]PostNode `json:"content"`
	}
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("feishu: parse post body: %w", err)
	}
	if block.Content == nil {
		block.Content = [][]PostNode{}
	}
	return &Post{Title: block.Title, Blocks: block.Content}, nil
}

// StripBotMentionsFromPost removes at-nodes whose user_id matches a Mention
// flagged IsBot, plus any "@_all" mention. Mirrors StripMentionPlaceholders'
// rules for text messages: the @bot trigger is not part of the request
// payload and would leak into the prompt as a stray "@<bot_name>" if left
// in place.
//
// Pure: mutates p in place but performs no IO. The Mentions list comes from
// the parsed event envelope (see bot_dispatch.go), where IsBot is set by
// the lark client from the wire-level mentioned_type / is_bot field.
func StripBotMentionsFromPost(p *Post, mentions []Mention) {
	if p == nil || len(mentions) == 0 {
		return
	}
	botIDs := make(map[string]bool, len(mentions))
	hasAll := false
	for _, m := range mentions {
		if m.IsBot && m.OpenID != "" {
			botIDs[m.OpenID] = true
		}
		if m.OpenID == "all" || m.Key == "@_all" {
			hasAll = true
		}
	}
	if len(botIDs) == 0 && !hasAll {
		return
	}
	for i, block := range p.Blocks {
		out := make([]PostNode, 0, len(block))
		for _, node := range block {
			if node.Tag == "at" {
				if node.UserID != "" && botIDs[node.UserID] {
					continue
				}
				if hasAll && (node.UserID == "all") {
					continue
				}
			}
			out = append(out, node)
		}
		p.Blocks[i] = out
	}
}

// RenderNodeToMarkdown renders one inline node to its Markdown form. The
// renderer is total: every supported tag returns a string (possibly "");
// unknown tags return "" so the dispatcher can compose paragraphs without
// per-tag branching.
//
// img and media are intentionally NOT handled here: the dispatcher must
// materialise the binary (img) or decide on a placeholder (media), so this
// function returns "" for both and the caller substitutes the right text.
// Keeping image/materialisation out of the pure renderer preserves the
// "feishu package has no side effects" boundary that makes it unit-testable
// without IO.
//
// at handling: @bot detection needs the bot's own user_id, which the feishu
// package does not know. This function renders every at as @<name>; the
// dispatcher post-processes the rendered paragraph to drop the @bot ones
// (matching StripMentionPlaceholders semantics for text messages).
func RenderNodeToMarkdown(node PostNode) string {
	switch node.Tag {
	case "text":
		return node.Text
	case "a":
		if node.Href == "" {
			return node.Text
		}
		if node.Text == "" {
			return node.Href
		}
		return fmt.Sprintf("[%s](%s)", node.Text, node.Href)
	case "at":
		name := node.UserName
		if name == "" {
			name = "用户"
		}
		return "@" + name
	case "emotion":
		return node.Emoji
	case "code_block":
		return "```" + node.Language + "\n" + node.Text + "\n```"
	case "img", "media":
		// Materialised by the dispatcher (img) or replaced with a placeholder
		// (media). Returning "" here lets the renderer be a pure function.
		return ""
	default:
		return ""
	}
}

// RenderPostToMarkdown walks the AST and returns the full Markdown body,
// with each top-level block separated by a blank line. The title (when
// present) is rendered as a level-1 heading on its own block.
//
// img and media nodes are rendered as placeholders ([图片] / [视频]) so the
// output is a complete Markdown string with no missing positions. The
// dispatcher uses RenderPostToMarkdown in the "file pipeline disabled" path
// (no image download); the enabled path walks Blocks itself so it can
// splice materialised image paths in place of the placeholders.
func RenderPostToMarkdown(p *Post) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	if p.Title != "" {
		b.WriteString("# ")
		b.WriteString(p.Title)
		b.WriteString("\n\n")
	}
	for i, block := range p.Blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		for _, node := range block {
			switch node.Tag {
			case "img":
				b.WriteString("[图片]")
			case "media":
				b.WriteString("[视频]")
			default:
				b.WriteString(RenderNodeToMarkdown(node))
			}
		}
	}
	return b.String()
}
