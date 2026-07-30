// Package strutil holds small string helpers shared across packages.
package strutil

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// Truncate shortens s to at most n bytes. If s is longer, the suffix
// "..." is appended so the total length is n+3. n must be > 0.
//
// The cut lands on a UTF-8 rune boundary so the result is always
// valid UTF-8 (a byte-boundary cut could split a multi-byte sequence
// in the middle of a Chinese character or emoji).
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// cut==0 means n is smaller than the first rune's byte length (e.g.
	// Truncate("你好", 1)); s[:n] would split a multi-byte sequence. Return
	// just the ellipsis so the result stays valid UTF-8.
	if cut == 0 {
		return "..."
	}
	return s[:cut] + "..."
}

// StringifyContent normalises a tool_result "content" field, which the
// CLI emits as either a plain string or an array of content blocks
// (e.g. [{"type":"text","text":"..."}]). Returns "" for nil/empty.
func StringifyContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" || blk.Type == "" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return strings.TrimSpace(string(raw))
}

// StringifyContentEnvelope first tries a {content:[...]} envelope, then
// falls back to a bare string / content-block array. Used by omp whose tool
// result wraps content in an extra envelope object.
func StringifyContentEnvelope(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// result: {content:[{type:"text",text:"..."}]}
	var envelope struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &envelope) == nil && len(envelope.Content) > 0 {
		var b strings.Builder
		for _, blk := range envelope.Content {
			if blk.Type == "text" || blk.Type == "" {
				b.WriteString(blk.Text)
			}
		}
		if b.Len() > 0 {
			return b.String()
		}
	}
	// Fallback: maybe a bare string or content-block array without envelope.
	return StringifyContent(raw)
}

// StringifyJSON returns a compacted JSON string for a raw input payload,
// or "" when empty. Used for tool_use input so the caller can render it
// without re-marshalling. json.Compact preserves the payload verbatim
// (key order, integer precision) where an unmarshal+marshal round trip
// would drop large ints to float64 and reorder keys.
func StringifyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return buf.String()
}
