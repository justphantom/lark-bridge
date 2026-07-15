package claudebridge

import (
	"strings"
	"testing"
)

func TestStripThinking(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "no think block",
			in:   "Hello world",
			want: "Hello world",
		},
		{
			name: "single think block removed",
			in:   "before<think>secret reasoning</think>after",
			// The block is replaced in-place with a "> 💭 …\n" blockquote.
			want: "before> 💭 secret reasoning\nafter",
		},
		{
			name: "empty think block dropped",
			in:   "a<think>   </think>b",
			want: "ab",
		},
		{
			name: "case-insensitive tags",
			in:   "x<THINK>reasoning here</THINK>y",
			want: "x> 💭 reasoning here\ny",
		},
		{
			name: "unterminated think dropped",
			in:   "visible<think>abandoned",
			want: "visible",
		},
		{
			name: "multiline thinking quoted line by line",
			in:   "a<think>line1\nline2</think>b",
			want: "a> 💭 line1\n> 💭 line2\nb",
		},
		{
			name: "no think keyword returns trimmed",
			in:   "  spaced  ",
			want: "spaced",
		},
		{
			name: "empty input",
			in:   "",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := stripThinking(tc.in)
			if got != tc.want {
				t.Errorf("stripThinking() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStripThinking_MultipleBlocks(t *testing.T) {
	in := "<think>first</think> middle <think>second</think> end"
	got := stripThinking(in)
	if !strings.Contains(got, "> 💭 first") {
		t.Errorf("missing first block: %q", got)
	}
	if !strings.Contains(got, "> 💭 second") {
		t.Errorf("missing second block: %q", got)
	}
	if !strings.Contains(got, "middle") || !strings.Contains(got, "end") {
		t.Errorf("non-think text dropped: %q", got)
	}
}
