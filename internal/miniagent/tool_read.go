package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// maxReadFileChars bounds one read_file result so a huge file cannot blow
// the LLM context window. Truncated with a marker.
const maxReadFileChars = 20000

// readfileArgs is the LLM-supplied argument object for read_file.
type readfileArgs struct {
	Path string `json:"path"`
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
	var a readfileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}

	var full string
	if r.Unrestricted {
		full = a.Path // no path bounds in free mode
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
	return ToolResult{Output: truncate(string(data), maxReadFileChars, "…")}
}
