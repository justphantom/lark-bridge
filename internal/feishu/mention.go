package feishu

import (
	"regexp"
	"sort"
	"strings"

	sdktypes "github.com/larksuite/oapi-sdk-go/v3/channel/types"
)

// horizSpaceRun matches a run of horizontal whitespace (spaces/tabs) so it
// can be collapsed to a single space without touching interior newlines.
var horizSpaceRun = regexp.MustCompile(`[ \t]+`)

// lineBreakSpace matches horizontal whitespace hugging a newline so it can
// be stripped, keeping wrapped lines clean after a mention is removed.
var lineBreakSpace = regexp.MustCompile(`[ \t]*\n[ \t]*`)

// StripMentionPlaceholders rewrites a Feishu text-type message by
// substituting "@_user_N" (and "@_all") placeholders using the
// SDK-parsed Mentions list. Feishu replaces every "@bot" in a text
// message with the positional placeholder "@_user_N" before delivery;
// the SDK only strips these placeholders for post (rich-text) messages,
// so text-type messages arrive with the raw placeholders intact. This
// helper closes that gap.
//
// Replacement rules:
//
//   - bot mentions (IsBot=true) are removed entirely along with one
//     trailing whitespace run; mentioning the bot is a trigger, not
//     part of the request.
//   - "@_all" / OpenID=="all" placeholders are removed along with one
//     trailing whitespace run (same trigger semantics).
//   - other user mentions become "@<Name>" (or "@用户" when Name is
//     empty), preserving the social context for the LLM without leaking
//     opaque open_id identifiers into the prompt.
//
// Placeholders are replaced longest-first: "@_user_1" is a string prefix
// of "@_user_10".."@_user_19", so a naive in-order ReplaceAll would
// corrupt "@_user_10" into "@<Name1>0". Sorting by descending key length
// guarantees longer placeholders are consumed before their prefixes.
//
// Whitespace is normalised afterwards: collapsed runs of horizontal
// whitespace become a single space, and leading/trailing whitespace is
// trimmed, while interior newlines are preserved so multi-line input
// (pasted code, paragraphs) reaches Claude with its structure intact.
//
// Bot detection relies on the SDK setting m.IsBot for the bot's own mention.
// (An earlier design also matched a preloaded bot OpenID, but that path was
// never wired to a live identity fetch — IsBot is sufficient in practice.)
func StripMentionPlaceholders(text string, mentions []sdktypes.Mention) string {
	if text == "" || len(mentions) == 0 {
		return text
	}
	// Copy and sort by descending Key length so "@_user_10" is replaced
	// before "@_user_1" (substring-prefix collision guard).
	sorted := make([]sdktypes.Mention, len(mentions))
	copy(sorted, mentions)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].Key) > len(sorted[j].Key)
	})
	for _, m := range sorted {
		if m.Key == "" {
			continue
		}
		if isAllMention(m) {
			text = removePlaceholder(text, m.Key)
			continue
		}
		if isBotMention(m) {
			text = removePlaceholder(text, m.Key)
			continue
		}
		text = strings.ReplaceAll(text, m.Key, "@"+pickMentionName(m))
	}
	return normalizeWhitespace(text)
}

// isAllMention matches the "@_all" / OpenID=="all" wildcard placeholder.
func isAllMention(m sdktypes.Mention) bool {
	return m.Key == "@_all" || m.OpenID == "all"
}

// isBotMention matches a mention that targets the running bot.
func isBotMention(m sdktypes.Mention) bool {
	return m.IsBot
}

// pickMentionName returns the user-visible name for a non-bot mention.
// Falls back to "用户" when Name is empty, so an opaque open_id is never
// injected into the Claude prompt.
func pickMentionName(m sdktypes.Mention) string {
	if m.Name != "" {
		return m.Name
	}
	return "用户"
}

// removePlaceholder strips the placeholder plus the immediately
// adjacent single whitespace character (one before, one after) when
// present, so removing a mention does not leave a double space behind.
// Whitespace is fully collapsed afterwards by normalizeWhitespace.
// All occurrences are removed; Feishu doesn't repeat the same mention
// in a single message, but the helper stays robust against malformed
// inputs.
func removePlaceholder(text, placeholder string) string {
	for {
		idx := strings.Index(text, placeholder)
		if idx < 0 {
			return text
		}
		end := idx + len(placeholder)
		if end < len(text) && (text[end] == ' ' || text[end] == '\t') {
			end++
		}
		start := idx
		if start > 0 && (text[start-1] == ' ' || text[start-1] == '\t') {
			start--
		}
		text = text[:start] + text[end:]
	}
}

// normalizeWhitespace collapses runs of horizontal whitespace (spaces and
// tabs) to a single space and trims leading/trailing whitespace, while
// preserving interior newlines. Whitespace hugging a newline is removed so
// mention removal never leaves trailing/leading spaces on wrapped lines.
// Newlines are deliberately kept so multi-line prompts (pasted code,
// paragraphs) are not flattened into a single line.
func normalizeWhitespace(s string) string {
	s = lineBreakSpace.ReplaceAllString(s, "\n")
	s = horizSpaceRun.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}
