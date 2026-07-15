package claudebridge

import (
	"encoding/json"
)

// summarizeToolInput extracts a human-readable description from a Claude
// tool_use JSON input. Instead of showing raw JSON like {"file_path":"..."},
// it extracts the key field (file_path, command, pattern, etc.) so the
// progress card shows "Read: /opt/codes/foo.go" instead of "Read: {...}".
func summarizeToolInput(input string) string {
	if input == "" || input == "{}" {
		return ""
	}
	// Try to parse as JSON and extract known fields.
	var m map[string]any
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		return input // not JSON, show as-is
	}
	// subject is a short title (TaskCreate); prefer it over description,
	// which is a long paragraph.
	if v, ok := m["subject"].(string); ok && v != "" {
		return v
	}
	// Priority: command > file_path > pattern > path > query > description,
	// plus tool identifiers (taskId for TaskUpdate, skill for Skill) and
	// MCP-specific fields (repo_path, project, url) so each tool renders its
	// most meaningful summary instead of falling through to a non-deterministic
	// first-string-value pick.
	for _, key := range []string{"command", "file_path", "pattern", "path", "query", "description", "taskId", "skill", "repo_path", "project", "url"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	// Fall back to the first string value in the object so an unrecognised
	// tool still shows something useful instead of raw JSON.
	if first := firstStringValue(m); first != "" {
		return first
	}
	return input
}

// firstStringValue returns the first non-empty string value encountered in a
// decoded JSON object, or "". Used as a last-resort tool-input summary so the
// progress card never shows a bare "{}" for an unrecognised tool.
func firstStringValue(m map[string]any) string {
	for _, v := range m {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}
