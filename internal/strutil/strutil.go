// Package strutil holds small string helpers shared across packages.
package strutil

import (
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
