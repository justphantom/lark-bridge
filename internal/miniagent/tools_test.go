package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp writes a file under a fresh temp dir and returns the dir.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return dir
}

// TestReadFile_RelativePath verifies a path relative to workspace_root reads.
func TestReadFile_RelativePath(t *testing.T) {
	dir := writeTemp(t, "a.txt", "hello world")
	r := ReadFile{WorkspaceRoot: dir}
	res := r.Call(context.Background(), `{"path":"a.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "hello world" {
		t.Errorf("Output = %q, want 'hello world'", res.Output)
	}
}

// TestReadFile_AbsoluteInsideRoot verifies an absolute path inside root reads.
func TestReadFile_AbsoluteInsideRoot(t *testing.T) {
	dir := writeTemp(t, "b.txt", "abs ok")
	r := ReadFile{WorkspaceRoot: dir}
	res := r.Call(context.Background(), `{"path":"`+filepath.Join(dir, "b.txt")+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "abs ok" {
		t.Errorf("Output = %q", res.Output)
	}
}

// TestReadFile_EscapeViaDotDot verifies a ../ escape is rejected.
func TestReadFile_EscapeViaDotDot(t *testing.T) {
	dir := writeTemp(t, "c.txt", "x")
	r := ReadFile{WorkspaceRoot: dir}
	res := r.Call(context.Background(), `{"path":"../../../etc/passwd"}`)
	if !res.IsError {
		t.Fatalf("expected error for escape, got Output=%q", res.Output)
	}
	if !strings.Contains(res.Output, "workspace_root") && !strings.Contains(res.Output, "越出") {
		t.Errorf("error = %q, want mentions workspace_root boundary", res.Output)
	}
}

// TestReadFile_AbsoluteOutsideRoot verifies an absolute path outside root is rejected.
func TestReadFile_AbsoluteOutsideRoot(t *testing.T) {
	dir := writeTemp(t, "d.txt", "x")
	other := t.TempDir()
	r := ReadFile{WorkspaceRoot: dir}
	res := r.Call(context.Background(), `{"path":"`+filepath.Join(other, "evil")+`"}`)
	if !res.IsError {
		t.Fatalf("expected error for outside-root path, got %q", res.Output)
	}
}

// TestReadFile_NotFound verifies a missing file yields IsError.
func TestReadFile_NotFound(t *testing.T) {
	r := ReadFile{WorkspaceRoot: t.TempDir()}
	res := r.Call(context.Background(), `{"path":"nope.txt"}`)
	if !res.IsError {
		t.Fatalf("expected error for missing file")
	}
}

// TestReadFile_IsDir verifies a directory path yields IsError.
func TestReadFile_IsDir(t *testing.T) {
	r := ReadFile{WorkspaceRoot: t.TempDir()}
	res := r.Call(context.Background(), `{"path":"."}`)
	if !res.IsError {
		t.Fatalf("expected error for directory")
	}
	if !strings.Contains(res.Output, "目录") {
		t.Errorf("error = %q, want mentions directory", res.Output)
	}
}

// TestReadFile_Truncates verifies a file over the limit is clamped with a marker.
func TestReadFile_Truncates(t *testing.T) {
	long := strings.Repeat("a", maxReadFileChars+500)
	dir := writeTemp(t, "big.txt", long)
	r := ReadFile{WorkspaceRoot: dir}
	res := r.Call(context.Background(), `{"path":"big.txt"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "已截断") {
		t.Errorf("Output missing truncation marker; len=%d", len(res.Output))
	}
}

// TestReadFile_EmptyWorkspaceRoot verifies an unset root errors without touching fs.
func TestReadFile_EmptyWorkspaceRoot(t *testing.T) {
	r := ReadFile{}
	res := r.Call(context.Background(), `{"path":"x"}`)
	if !res.IsError {
		t.Fatal("expected error for empty workspace_root")
	}
}

// TestReadFile_BadArgs verifies malformed JSON args yield IsError.
func TestReadFile_BadArgs(t *testing.T) {
	r := ReadFile{WorkspaceRoot: t.TempDir()}
	res := r.Call(context.Background(), `not json`)
	if !res.IsError {
		t.Fatal("expected error for malformed args")
	}
}

// TestReadFile_MissingPath verifies a JSON without path yields IsError.
func TestReadFile_MissingPath(t *testing.T) {
	r := ReadFile{WorkspaceRoot: t.TempDir()}
	res := r.Call(context.Background(), `{}`)
	if !res.IsError {
		t.Fatal("expected error for missing path")
	}
}

// TestReadFile_Spec verifies the schema has the tool name and required path.
func TestReadFile_Spec(t *testing.T) {
	spec := ReadFile{}.Spec()
	if spec.Name != "read_file" {
		t.Errorf("Name = %q, want read_file", spec.Name)
	}
	props, ok := spec.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("parameters.properties missing or wrong type")
	}
	if _, ok := props["path"]; !ok {
		t.Error("schema missing path property")
	}
}
