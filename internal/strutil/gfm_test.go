package strutil

import "testing"

func TestGfmCellEscape(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain ascii", "hello", "hello"},
		{"empty", "", ""},
		{"whitespace only → empty", "   \t  ", ""},
		{"tab inside kept", "a\tb", "a\tb"},
		{"pipe escaped", "a|b", "a\\|b"},
		{"multiple pipes", "|x|y|", "\\|x\\|y\\|"},
		{"newline to br", "line1\nline2", "line1<br>line2"},
		{"crlf to single br", "line1\r\nline2", "line1<br>line2"},
		{"lone cr dropped", "a\rb\rc", "abc"},
		{"mixed newlines and pipes", "a|b\nc|d", "a\\|b<br>c\\|d"},
		{"chinese unaffected", "你好世界", "你好世界"},
		{"leading trailing spaces kept when non-empty", "  hi  ", "  hi  "},
		{"pipe then newline order", "x|\ny", "x\\|<br>y"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GfmCellEscape(tt.in); got != tt.want {
				t.Errorf("GfmCellEscape(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
