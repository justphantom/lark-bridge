package bridgebase

import (
	"encoding/json"
	"strconv"
)

// SummarizeToolInput extracts a human-readable description from a tool_use
// JSON input. Instead of showing raw JSON like {"file_path":"..."}, it
// extracts the key field so the progress card shows "Read: /opt/codes/foo.go"
// instead of "Read: {...}". Covers both naming generations: claude's
// snake_case native tools (file_path) and opencode's camelCase ones
// (filePath); MCP tools pass through server-defined params in either shape.
//
// toolName routes tool-specific summarisation: todowrite carries a todos
// array (no single value conveys progress), so it folds to "清单 N/M"
// (N=non-pending, M=total). The match is exact-equality — todoread is a
// different tool and must NOT take this path.
func SummarizeToolInput(toolName, input string) string {
	if input == "" || input == "{}" {
		return ""
	}
	// Try to parse as JSON and extract known fields.
	var m map[string]any
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		return input // not JSON, show as-is
	}
	if toolName == "todowrite" {
		if summary := summarizeTodoWrite(m); summary != "" {
			return summary
		}
		// Malformed todowrite input (no usable todos array): fall through
		// to the generic path so the card still shows something.
	}
	// subject is a short title (TaskCreate); prefer it over description,
	// which is a long paragraph.
	if v, ok := m["subject"].(string); ok && v != "" {
		return v
	}
	// Priority: command > file path > pattern > path > query > description,
	// plus tool identifiers (taskId for TaskUpdate, skill for Skill) and
	// MCP-specific fields (repo_path, project, url) so each tool renders its
	// most meaningful summary instead of falling through to a non-deterministic
	// first-string-value pick.
	for _, key := range []string{"command", "file_path", "filePath", "pattern", "path", "query", "description", "prompt", "taskId", "skill", "repo_path", "repoPath", "project", "url"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	// Fall back to the first string value in the object so an unrecognised
	// tool still shows something useful instead of raw JSON.
	for _, v := range m {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return input
}

// summarizeTodoWrite folds a todowrite tool input `{"todos":[...]}` into
// "清单 N/M" (N=non-pending, M=total). Returns "" when there is no usable
// todos array, so the caller can fall back to the generic summary path.
func summarizeTodoWrite(m map[string]any) string {
	arr, ok := m["todos"].([]any)
	if !ok || len(arr) == 0 {
		return ""
	}
	nonPending := 0
	for _, raw := range arr {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		status, _ := item["status"].(string)
		if status != "pending" {
			nonPending++
		}
	}
	return "清单 " + strconv.Itoa(nonPending) + "/" + strconv.Itoa(len(arr))
}
