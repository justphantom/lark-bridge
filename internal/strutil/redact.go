package strutil

import "fmt"

// DebugRedact optionally redacts sensitive text from debug logs.
// When redact is true, replaces the entire string with "<redacted N bytes>"
// where N is the length of the original string. This preserves length
// information for debugging while hiding content. When redact is false,
// returns the original string unchanged (default behavior).
func DebugRedact(s string, redact bool) string {
	if !redact {
		return s
	}
	return fmt.Sprintf("<redacted %d bytes>", len(s))
}
