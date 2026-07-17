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

// === Shell tests ===

// TestShell_RunsCommand verifies a plain command executes and returns output.
func TestShell_RunsCommand(t *testing.T) {
	s := Shell{WorkspaceRoot: t.TempDir()}
	res := s.Call(context.Background(), `{"command":"echo hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q, want contains 'hello'", res.Output)
	}
}

// TestShell_CwdIsWorkspaceRoot verifies cwd is the workspace root: a pwd
// lands inside it.
func TestShell_CwdIsWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	s := Shell{WorkspaceRoot: dir}
	res := s.Call(context.Background(), `{"command":"pwd"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	// pwd resolves the absolute dir; match by suffix (TempDir may have macOS
	// /var -> /private/var or linux symlinks, so HasSuffix on Cleaned dir).
	cleaned := filepath.Clean(dir)
	if !strings.Contains(res.Output, cleaned) {
		t.Errorf("Output = %q, want contains %q", res.Output, cleaned)
	}
}

// TestShell_BlockedPattern verifies rm -rf is refused before exec.
func TestShell_BlockedPattern(t *testing.T) {
	s := Shell{WorkspaceRoot: t.TempDir()}
	res := s.Call(context.Background(), `{"command":"rm -rf /"}`)
	if !res.IsError {
		t.Fatal("expected error for blocked pattern")
	}
	if !strings.Contains(res.Output, "黑名单") {
		t.Errorf("error = %q, want mentions 黑名单", res.Output)
	}
}

// TestShell_BlockedPatternCaseInsensitive verifies matching is case-insensitive.
func TestShell_BlockedPatternCaseInsensitive(t *testing.T) {
	s := Shell{WorkspaceRoot: t.TempDir()}
	res := s.Call(context.Background(), `{"command":"RM -RF /tmp/x"}`)
	if !res.IsError {
		t.Fatal("expected error for upper-case blocked pattern")
	}
}

// TestShell_NonZeroExitIsError verifies a failing command yields IsError with
// the output preserved (LLM needs stderr to diagnose).
func TestShell_NonZeroExitIsError(t *testing.T) {
	s := Shell{WorkspaceRoot: t.TempDir()}
	res := s.Call(context.Background(), `{"command":"echo out; exit 3"}`)
	if !res.IsError {
		t.Fatal("expected IsError for non-zero exit")
	}
	if !strings.Contains(res.Output, "out") {
		t.Errorf("Output = %q, want contains stdout 'out'", res.Output)
	}
	if !strings.Contains(res.Output, "退出码") {
		t.Errorf("Output = %q, want mentions 退出码", res.Output)
	}
}

// TestShell_EmptyWorkspaceRoot verifies unset root errors without exec.
func TestShell_EmptyWorkspaceRoot(t *testing.T) {
	s := Shell{}
	res := s.Call(context.Background(), `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected error for empty workspace_root")
	}
}

// TestShell_EmptyCommand verifies an empty/whitespace command errors.
func TestShell_EmptyCommand(t *testing.T) {
	s := Shell{WorkspaceRoot: t.TempDir()}
	res := s.Call(context.Background(), `{"command":"   "}`)
	if !res.IsError {
		t.Fatal("expected error for empty command")
	}
}

// TestShell_BadArgs verifies malformed JSON errors.
func TestShell_BadArgs(t *testing.T) {
	s := Shell{WorkspaceRoot: t.TempDir()}
	res := s.Call(context.Background(), `not json`)
	if !res.IsError {
		t.Fatal("expected error for malformed args")
	}
}

// TestShell_Spec verifies the schema.
func TestShell_Spec(t *testing.T) {
	spec := Shell{}.Spec()
	if spec.Name != "shell" {
		t.Errorf("Name = %q, want shell", spec.Name)
	}
}

// === WriteFile tests ===

// TestWriteFile_CreatesNew verifies a fresh file is written with the content.
func TestWriteFile_CreatesNew(t *testing.T) {
	dir := t.TempDir()
	w := WriteFile{WorkspaceRoot: dir}
	res := w.Call(context.Background(), `{"path":"a.txt","content":"hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q, want 'hello'", got)
	}
}

// TestWriteFile_OverwritesExisting verifies a second write replaces content.
func TestWriteFile_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	w := WriteFile{WorkspaceRoot: dir}
	if res := w.Call(context.Background(), `{"path":"b.txt","content":"old"}`); res.IsError {
		t.Fatalf("first write: %s", res.Output)
	}
	if res := w.Call(context.Background(), `{"path":"b.txt","content":"new"}`); res.IsError {
		t.Fatalf("second write: %s", res.Output)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "b.txt"))
	if string(got) != "new" {
		t.Errorf("content = %q, want 'new' (overwrite)", got)
	}
}

// TestWriteFile_CreatesParentDirs verifies missing nested dirs are made.
func TestWriteFile_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	w := WriteFile{WorkspaceRoot: dir}
	res := w.Call(context.Background(), `{"path":"src/nested/deep/c.go","content":"x"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if _, err := os.Stat(filepath.Join(dir, "src", "nested", "deep", "c.go")); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

// TestWriteFile_EscapeRejected verifies ../ outside root is refused.
func TestWriteFile_EscapeRejected(t *testing.T) {
	dir := t.TempDir()
	w := WriteFile{WorkspaceRoot: dir}
	res := w.Call(context.Background(), `{"path":"../../../tmp/evil","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected error for escape, got success")
	}
}

// TestWriteFile_AbsoluteOutsideRejected verifies an abs path outside root.
func TestWriteFile_AbsoluteOutsideRejected(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	w := WriteFile{WorkspaceRoot: dir}
	res := w.Call(context.Background(), `{"path":"`+filepath.Join(other, "x")+`","content":"x"}`)
	if !res.IsError {
		t.Fatal("expected error for outside-root absolute path")
	}
}

// TestWriteFile_EmptyWorkspaceRoot errors.
func TestWriteFile_EmptyWorkspaceRoot(t *testing.T) {
	w := WriteFile{}
	res := w.Call(context.Background(), `{"path":"x","content":"y"}`)
	if !res.IsError {
		t.Fatal("expected error for empty workspace_root")
	}
}

// TestWriteFile_BadArgs errors.
func TestWriteFile_BadArgs(t *testing.T) {
	w := WriteFile{WorkspaceRoot: t.TempDir()}
	res := w.Call(context.Background(), `not json`)
	if !res.IsError {
		t.Fatal("expected error for malformed args")
	}
}

// TestWriteFile_MissingPath errors.
func TestWriteFile_MissingPath(t *testing.T) {
	w := WriteFile{WorkspaceRoot: t.TempDir()}
	res := w.Call(context.Background(), `{"content":"y"}`)
	if !res.IsError {
		t.Fatal("expected error for missing path")
	}
}

// TestWriteFile_FileMode0644 verifies the default mode.
func TestWriteFile_FileMode0644(t *testing.T) {
	dir := t.TempDir()
	w := WriteFile{WorkspaceRoot: dir}
	if res := w.Call(context.Background(), `{"path":"m.txt","content":"x"}`); res.IsError {
		t.Fatalf("write: %s", res.Output)
	}
	info, err := os.Stat(filepath.Join(dir, "m.txt"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("mode = %o, want 0644", got)
	}
}

// TestWriteFile_Spec verifies the schema name + required fields.
func TestWriteFile_Spec(t *testing.T) {
	spec := WriteFile{}.Spec()
	if spec.Name != "write_file" {
		t.Errorf("Name = %q, want write_file", spec.Name)
	}
	props, ok := spec.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("parameters.properties missing")
	}
	for _, k := range []string{"path", "content"} {
		if _, ok := props[k]; !ok {
			t.Errorf("schema missing %q property", k)
		}
	}
}
