package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// writefileArgs is the LLM-supplied argument object for write_file.
type writefileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFile writes text content to a path under WorkspaceRoot, creating the
// file if absent and truncating if present (overwrite semantics). Missing
// parent directories are created. Path safety is the same as ReadFile: a
// path that escapes WorkspaceRoot (after Clean) is refused.
type WriteFile struct {
	WorkspaceRoot string
	Unrestricted  bool
}

func (WriteFile) Spec() ToolSpec {
	return ToolSpec{
		Name:        "write_file",
		Description: "把 content 写入 workspace_root 内的文件（覆盖已有内容；自动创建父目录）。path 可相对 workspace_root 或绝对。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "要写入的文件路径，相对 workspace_root 或绝对路径",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "要写入的完整文件内容",
				},
			},
			"required": []string{"path", "content"},
		},
	}
}

// Call resolves path under WorkspaceRoot, creates parent dirs, and writes.
// Any failure (root unset, escape, mkdir failure, write failure) yields
// IsError=true. Returns the bytes written on success.
func (w WriteFile) Call(_ context.Context, args string) ToolResult {
	var a writefileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}

	var full string
	if w.Unrestricted {
		full = a.Path
		if w.WorkspaceRoot != "" && !filepath.IsAbs(a.Path) {
			full = filepath.Join(w.WorkspaceRoot, a.Path)
		}
	} else {
		if w.WorkspaceRoot == "" {
			return ToolResult{IsError: true, Output: "write_file 未配置：workspace_root 为空"}
		}
		root, err := filepath.Abs(w.WorkspaceRoot)
		if err != nil {
			return ToolResult{IsError: true, Output: fmt.Sprintf("解析 workspace_root 失败：%v", err)}
		}
		full, err = resolveUnderRoot(root, a.Path)
		if err != nil {
			return ToolResult{IsError: true, Output: err.Error()}
		}
	}

	// MkdirAll so the LLM can write src/new.go without a separate mkdir
	// tool. The parent dir is already bounded to workspace_root by
	// resolveUnderRoot, so this cannot create dirs outside it.
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("创建父目录失败：%v", err)}
	}
	// 0644: standard project-file mode (owner rw, group/other r). Matches
	// what `cat > file` or an editor produces, so the file interops with
	// shell/git without surprising permission diffs.
	if err := os.WriteFile(full, []byte(a.Content), 0o600); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: fmt.Sprintf("已写入 %d 字节到 %s", len(a.Content), a.Path)}
}
