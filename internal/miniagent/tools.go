package miniagent

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Tool is one agent tool the LLM may call. Spec returns the OpenAI
// function schema (name/description/parameters); Call executes it with the
// raw JSON arguments string the LLM produced.
type Tool interface {
	Spec() ToolSpec
	Call(ctx context.Context, args string) ToolResult
}

// ToolResult is the outcome of one tool call. Output is fed back to the
// LLM verbatim as the tool message content; IsError tells the LLM the call
// failed (it should not parse Output as success data).
type ToolResult struct {
	Output  string
	IsError bool
}

// resolveUnderRoot cleans p and ensures the result stays under root. It
// accepts an absolute path inside root or a path relative to root. A path
// that escapes root via ".." or is absolute but outside root returns an
// error naming the offending path.
func resolveUnderRoot(root, p string) (string, error) {
	clean := filepath.Clean(p)
	var full string
	if filepath.IsAbs(clean) {
		full = clean
	} else {
		full = filepath.Join(root, clean)
	}
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return "", fmt.Errorf("路径 %q 不在 workspace_root 内", p)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径 %q 越出 workspace_root", p)
	}
	return full, nil
}

// truncate clamps s to n runes and appends marker when it truncated. rune-
// based so multibyte content (中文) is never split mid-character. n<=0 means
// no limit. marker is "" for a silent cut, or a visible suffix like "…".
func truncate(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + marker
}
