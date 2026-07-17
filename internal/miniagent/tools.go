package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxReadFileChars bounds one read_file result so a huge file cannot blow
// the LLM context window or stall the turn. The tail is dropped; a marker
// is appended so the LLM knows it saw a truncated view.
const maxReadFileChars = 20000

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

// readfileArgs is the LLM-supplied argument object for read_file.
type readfileArgs struct {
	Path string `json:"path"`
}

// ReadFile reads a text file under WorkspaceRoot. It is the P1a tool;
// write_file / shell / webfetch land later. Call enforces WorkspaceRoot:
// a path that escapes it (after Clean) returns an error result instead of
// touching the filesystem.
type ReadFile struct {
	WorkspaceRoot string // absolute or relative; cleaned in Call
}

// Spec returns the OpenAI function schema advertised to the LLM.
func (ReadFile) Spec() ToolSpec {
	return ToolSpec{
		Name:        "read_file",
		Description: "读取 workspace_root 内的文本文件内容。path 可以是绝对路径或相对 workspace_root 的路径。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要读取的文件路径，相对 workspace_root 或绝对路径",
				},
			},
			"required": []string{"path"},
		},
	}
}

// Call resolves path under WorkspaceRoot, reads the file, and truncates.
// Any failure (root unset, escape attempt, missing file, not a regular
// file, read error) yields IsError=true with a human-readable Output.
func (r ReadFile) Call(_ context.Context, args string) ToolResult {
	if strings.TrimSpace(r.WorkspaceRoot) == "" {
		return ToolResult{IsError: true, Output: "read_file 未配置：workspace_root 为空"}
	}
	var a readfileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}

	root, err := filepath.Abs(r.WorkspaceRoot)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("解析 workspace_root 失败：%v", err)}
	}
	full, err := resolveUnderRoot(root, a.Path)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}

	info, err := os.Stat(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("无法访问 %q：%v", a.Path, err)}
	}
	if info.IsDir() {
		return ToolResult{IsError: true, Output: fmt.Sprintf("%q 是目录，不是文件", a.Path)}
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: truncateToLimit(string(data), maxReadFileChars)}
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

// truncateToLimit clamps s to n runes and appends a marker when it truncated.
func truncateToLimit(s string, n int) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "\n…(已截断，共 " + fmt.Sprintf("%d", len(r)) + " 字符)"
}
