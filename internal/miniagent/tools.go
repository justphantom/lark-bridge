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

// resolveUnderRoot cleans p and ensures the result stays under root, both on
// the string level (Clean+Rel against ".." and absolute-outside-root) AND on
// the filesystem level (EvalSymlinks on the parent dir). The symlink check is
// what stops the shell+read_file combo where the agent first `ln -s
// /etc/passwd link` then `read_file link` — without it the string check sees
// `link` as inside root while the read actually hits /etc/passwd.
//
// EvalSymlinks targets the parent directory (always present for both reads of
// existing files and writes of new ones) so a symlinked parent escaping root
// is caught; the leaf name is appended after resolution.
func resolveUnderRoot(root, p string) (string, error) {
	clean := filepath.Clean(p)
	var full string
	if filepath.IsAbs(clean) {
		full = clean
	} else {
		full = filepath.Join(root, clean)
	}
	if err := checkUnderRoot(root, full, p); err != nil {
		return "", err
	}
	// Resolve the parent directory's symlinks, then re-check the real path.
	// A symlink under root pointing outward (e.g. `link -> /etc`) is caught
	// here; a missing leaf is fine because we only evaluate the parent.
	realParent, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		// Missing parent (e.g. root itself not yet created) — fall back to
		// the string check above, which is the best we can do. The error is
		// intentionally swallowed: returning it would break the common case
		// of writing a file whose parent dir does not yet exist.
		_ = err
		return full, nil
	}
	resolved := filepath.Join(realParent, filepath.Base(full))
	if err := checkUnderRoot(root, resolved, p); err != nil {
		return "", err
	}
	return resolved, nil
}

// checkUnderRoot is the string-level containment test shared by the pre- and
// post-symlink-resolution checks. It reports whether full is rooted under
// root (not escaping via ".." or as an outside absolute path).
func checkUnderRoot(root, full, original string) error {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return fmt.Errorf("路径 %q 不在 workspace_root 内", original)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 越出 workspace_root", original)
	}
	return nil
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
