package strutil

import (
	"testing"
)

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		s    string
		n    int
		want string
	}{
		{"empty cut", "abcdef", 0, ""},
		{"negative cut", "abcdef", -1, ""},
		{"shorter than n", "ab", 5, "ab"},
		{"equal length", "abc", 3, "abc"},
		{"ascii cut", "abcde", 3, "abc..."},
		{"rune boundary", "你好世界", 3, "你..."},                     // 3 bytes = 1st rune
		{"mid-rune cut backs off", "你好世界", 4, "你..."},            // 4 lands mid-2nd-rune → back to 3
		{"n below first rune → ellipsis only", "你好世界", 1, "..."}, // 1 < 3-byte rune
		{"n below first rune 2", "你好世界", 2, "..."},               // 2 < 3-byte rune
		{"emoji", "😀😀", 4, "😀..."},                               // 4-byte rune
		{"n below 4-byte emoji", "😀😀", 1, "..."},                 // 1 < 4-byte rune
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Truncate(tt.s, tt.n); got != tt.want {
				t.Errorf("Truncate(%q,%d) = %q, want %q", tt.s, tt.n, got, tt.want)
			}
		})
	}
}

func TestDebugRedact(t *testing.T) {
	if got := DebugRedact("secret", false); got != "secret" {
		t.Errorf("DebugRedact(redact=false) = %q, want %q", got, "secret")
	}
	if got := DebugRedact("secret", true); got != "<redacted 6 bytes>" {
		t.Errorf("DebugRedact(redact=true) = %q, want %q", got, "<redacted 6 bytes>")
	}
}

func TestExpandEnvVars(t *testing.T) {
	t.Setenv("LB_TEST_VAR", "hello")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"no vars", "plain text", "plain text"},
		{"single var", "${LB_TEST_VAR}", "hello"},
		{"var in middle", "pre-${LB_TEST_VAR}-post", "pre-hello-post"},
		{"unset var left untouched", "${LB_NOPE}", "${LB_NOPE}"},
		{"empty value expanded", "${LB_TEST_VAR}-x", "hello-x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandEnvVars(tt.in); got != tt.want {
				t.Errorf("ExpandEnvVars(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
