package bridgebase

import (
	"regexp"
	"strings"
)

// thinkPattern matches <think>...</think> reasoning blocks (case-insensitive,
// dot-matches-newline). Captured so the thinking trace can be surfaced as a
// blockquote rather than dropped silently.
var thinkPattern = regexp.MustCompile(`(?is)<think>(.*?)</think>`)

// StripThinking removes <think>…</think> blocks from a reply, converting each
// to a blockquote so the user can optionally expand the reasoning without it
// dominating the answer. quotePrefix is written before each quoted line
// ("> 💭 " for Claude, "> " for the other backends). Stray unmatched <think>
// tags (a truncated stream) are dropped.
func StripThinking(s, quotePrefix string) string {
	if !strings.Contains(strings.ToLower(s), "<think>") {
		return strings.TrimSpace(s)
	}
	converted := thinkPattern.ReplaceAllStringFunc(s, func(block string) string {
		m := thinkPattern.FindStringSubmatch(block)
		if len(m) < 2 || strings.TrimSpace(m[1]) == "" {
			return ""
		}
		// Quote each line of the thinking trace.
		var b strings.Builder
		for _, line := range strings.Split(strings.TrimSpace(m[1]), "\n") {
			b.WriteString(quotePrefix)
			b.WriteString(line)
			b.WriteByte('\n')
		}
		return b.String()
	})
	// Drop an unterminated <think> with no closing tag. Search case-
	// insensitively; ASCII lowercase preserves byte offsets so the slice on
	// the original string is correct.
	if open := strings.Index(strings.ToLower(converted), "<think>"); open >= 0 {
		converted = converted[:open]
	}
	return strings.TrimSpace(converted)
}
