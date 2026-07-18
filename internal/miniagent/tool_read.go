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
// the LLM context window. Truncated with a marker.
const maxReadFileChars = 20000

// readfileArgs is the LLM-supplied argument object for read_file.
type readfileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 1-based start line; 0 = from beginning
	Limit  int    `json:"limit,omitempty"`  // max lines to return; 0 = all
}

// ReadFile reads a text file under WorkspaceRoot. Call enforces
// WorkspaceRoot: a path that escapes it (after Clean) returns an error
// result instead of touching the filesystem. When Unrestricted is true
// (security_level="free"), path checks are skipped and any path the
// process user can access is readable.
type ReadFile struct {
	WorkspaceRoot string
	Unrestricted  bool
}

func (ReadFile) Spec() ToolSpec {
	return ToolSpec{
		Name:        "read_file",
		Description: "读取 workspace_root 内的文本文件内容。支持 offset/limit 按行范围读取，输出带行号标注。path 可以是绝对路径或相对 workspace_root 的路径。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要读取的文件路径，相对 workspace_root 或绝对路径",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "起始行号（1-based），默认 1（从头开始）",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "最多返回的行数，默认全部",
				},
			},
			"required": []string{"path"},
		},
	}
}

// Call resolves path under WorkspaceRoot, reads the file, and returns the
// content with line numbers. When offset/limit are provided, only the
// specified line range is returned. Any failure yields IsError=true.
func (r ReadFile) Call(_ context.Context, args string) ToolResult {
	var a readfileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}
	if a.Offset < 0 {
		a.Offset = 0
	}

	var full string
	if r.Unrestricted {
		full = a.Path
		// In free mode, resolve relative paths against WorkspaceRoot so /cd
		// still applies. Absolute paths are used as-is.
		if r.WorkspaceRoot != "" && !filepath.IsAbs(a.Path) {
			full = filepath.Join(r.WorkspaceRoot, a.Path)
		}
	} else {
		if r.WorkspaceRoot == "" {
			return ToolResult{IsError: true, Output: "read_file 未配置：workspace_root 为空"}
		}
		root, err := filepath.Abs(r.WorkspaceRoot)
		if err != nil {
			return ToolResult{IsError: true, Output: fmt.Sprintf("解析 workspace_root 失败：%v", err)}
		}
		full, err = resolveUnderRoot(root, a.Path)
		if err != nil {
			return ToolResult{IsError: true, Output: err.Error()}
		}
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

	content := string(data)

	// If no offset/limit, return raw content truncated (backward compat).
	if a.Offset == 0 && a.Limit == 0 {
		return ToolResult{Output: truncate(content, maxReadFileChars, "…")}
	}

	// Range read: split by lines, apply offset/limit, prepend line numbers.
	lines := strings.Split(content, "\n")
	start := a.Offset
	if start < 1 {
		start = 1
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit-1 < end {
		end = start + a.Limit - 1
	}
	if start > len(lines) {
		return ToolResult{IsError: true, Output: fmt.Sprintf("offset %d 超出文件行数（共 %d 行）", start, len(lines))}
	}

	var sb strings.Builder
	// Width for line number padding.
	width := len(fmt.Sprintf("%d", end))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%*d │ %s\n", width, i, lines[i-1])
	}
	return ToolResult{Output: truncate(sb.String(), maxReadFileChars, "…")}
}
