package strutil

import (
	"encoding/json"
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

func TestStringifyContent(t *testing.T) {
	t.Run("plain string", func(t *testing.T) {
		if got := StringifyContent(json.RawMessage(`"hello"`)); got != "hello" {
			t.Errorf("string = %q", got)
		}
	})
	t.Run("content-block array", func(t *testing.T) {
		arr := json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
		if got := StringifyContent(arr); got != "ab" {
			t.Errorf("array = %q", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := StringifyContent(nil); got != "" {
			t.Errorf("empty = %q", got)
		}
	})
	t.Run("non-JSON fallback", func(t *testing.T) {
		if got := StringifyContent(json.RawMessage(`plain text`)); got != "plain text" {
			t.Errorf("fallback = %q", got)
		}
	})
	t.Run("mixed type blocks", func(t *testing.T) {
		arr := json.RawMessage(`[{"type":"text","text":"hello"},{"type":"image","text":"skip"},{"type":"text","text":" world"}]`)
		if got := StringifyContent(arr); got != "hello world" {
			t.Errorf("mixed = %q", got)
		}
	})
}

func TestStringifyContentEnvelope(t *testing.T) {
	t.Run("envelope", func(t *testing.T) {
		raw := json.RawMessage(`{"content":[{"type":"text","text":"hello"},{"type":"text","text":" world"}]}`)
		if got := StringifyContentEnvelope(raw); got != "hello world" {
			t.Errorf("envelope = %q", got)
		}
	})
	t.Run("bare string fallback", func(t *testing.T) {
		if got := StringifyContentEnvelope(json.RawMessage(`"hello"`)); got != "hello" {
			t.Errorf("bare string = %q", got)
		}
	})
	t.Run("bare block array fallback", func(t *testing.T) {
		arr := json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
		if got := StringifyContentEnvelope(arr); got != "ab" {
			t.Errorf("bare array = %q", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := StringifyContentEnvelope(nil); got != "" {
			t.Errorf("empty = %q", got)
		}
	})
	t.Run("envelope with empty content falls back", func(t *testing.T) {
		raw := json.RawMessage(`{"content":[]}`)
		// Empty content array: envelope parsed, no text blocks extracted,
		// falls through to StringifyContent which returns the raw JSON.
		if got := StringifyContentEnvelope(raw); got != `{"content":[]}` {
			t.Errorf("empty envelope = %q, want raw fallback", got)
		}
	})
}

func TestStringifyJSON(t *testing.T) {
	t.Run("compacted JSON", func(t *testing.T) {
		if got := StringifyJSON(json.RawMessage(`{"a": 1}`)); got != `{"a":1}` {
			t.Errorf("compact = %q", got)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := StringifyJSON(nil); got != "" {
			t.Errorf("empty = %q", got)
		}
	})
	t.Run("non-JSON fallback", func(t *testing.T) {
		if got := StringifyJSON(json.RawMessage(`plain`)); got != "plain" {
			t.Errorf("fallback = %q", got)
		}
	})
	t.Run("preserves key order", func(t *testing.T) {
		// json.Compact preserves the original bytes, so no reordering.
		if got := StringifyJSON(json.RawMessage(`{"z":1,"a":2}`)); got != `{"z":1,"a":2}` {
			t.Errorf("order = %q", got)
		}
	})
}
