//go:build linux || darwin

package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newSessionsClient(t *testing.T, root string) *Client {
	t.Helper()
	return &Client{settingsDir: root}
}

// writeFile writes body to rel under root, creating parents as needed.
func writeFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeSessionJSONL writes a session transcript whose first line is a
// queue-operation record carrying content as the opening prompt.
func writeSessionJSONL(t *testing.T, root, dir, id, content string) {
	t.Helper()
	encoded := encodeProjectDir(dir)
	rel := filepath.Join("projects", encoded, id+".jsonl")
	line := `{"type":"queue-operation","operation":"enqueue","sessionId":"` + id + `","content":"` + content + `"}` + "\n"
	writeFile(t, root, rel, line)
}

func TestEncodeProjectDir(t *testing.T) {
	cases := map[string]string{
		"/home/user/lark-bridge":    "-home-user-lark-bridge",
		"/tmp/lb-session-test-VK1C": "-tmp-lb-session-test-VK1C",
		"/":                         "-",
		"/a":                        "-a",
	}
	for in, want := range cases {
		if got := encodeProjectDir(in); got != want {
			t.Errorf("encodeProjectDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestListSessions_EnumeratesAndSortsByMtime(t *testing.T) {
	root := t.TempDir()
	dir := "/home/user/proj"
	c := newSessionsClient(t, root)
	older := "00000000-0000-0000-0000-000000000001"
	newer := "00000000-0000-0000-0000-000000000002"
	writeSessionJSONL(t, root, dir, older, "first prompt")
	// Touch newer's mtime forward so it sorts first regardless of FS ordering.
	writeSessionJSONL(t, root, dir, newer, "second prompt")
	newerPath := filepath.Join(root, "projects", encodeProjectDir(dir), newer+".jsonl")
	at, mt := futureTime()
	if err := os.Chtimes(newerPath, at, mt); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got, err := c.ListSessions(context.Background(), dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("sessions = %d, want 2: %+v", len(got), got)
	}
	if got[0].ID != newer {
		t.Errorf("first = %s, want %s (newest first)", got[0].ID, newer)
	}
	if got[0].Title != "second prompt" {
		t.Errorf("first title = %q, want %q", got[0].Title, "second prompt")
	}
	if got[1].ID != older {
		t.Errorf("second = %s, want %s", got[1].ID, older)
	}
}

func TestListSessions_EmptyDir(t *testing.T) {
	root := t.TempDir()
	c := newSessionsClient(t, root)
	got, err := c.ListSessions(context.Background(), "/nowhere")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got != nil {
		t.Errorf("sessions = %+v, want nil for missing bucket", got)
	}
}

func TestListSessions_FiltersNonUUIDFiles(t *testing.T) {
	root := t.TempDir()
	dir := "/home/user/p"
	c := newSessionsClient(t, root)
	writeSessionJSONL(t, root, dir, "00000000-0000-0000-0000-00000000000a", "real")
	// A non-UUID jsonl and a stray non-jsonl file must be ignored.
	encoded := encodeProjectDir(dir)
	writeFile(t, root, filepath.Join("projects", encoded, "not-a-uuid.jsonl"), "{}")
	writeFile(t, root, filepath.Join("projects", encoded, "README"), "noise")
	got, err := c.ListSessions(context.Background(), dir)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 1 || got[0].ID != "00000000-0000-0000-0000-00000000000a" {
		t.Fatalf("got = %+v, want only the UUID session", got)
	}
}

func TestListSessions_RelativeDirResolved(t *testing.T) {
	root := t.TempDir()
	// Create a real subdir under temp and use its absolute form as the cwd.
	realDir := filepath.Join(root, "proj")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newSessionsClient(t, root)
	writeSessionJSONL(t, root, realDir, "00000000-0000-0000-0000-0000000000ff", "hi")
	// Pass a relative path; filepath.Abs resolves it against the process cwd,
	// which won't match realDir — encode differs, so no sessions surface. This
	// proves the driver resolves via filepath.Abs (encoding is path-derived).
	got, err := c.ListSessions(context.Background(), "rel-dir")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("relative dir should not match absolute bucket; got %+v", got)
	}
}

func TestDeleteSession_RemovesJSONLAndSidecar(t *testing.T) {
	root := t.TempDir()
	dir := "/home/user/p2"
	c := newSessionsClient(t, root)
	id := "11111111-1111-1111-1111-111111111111"
	writeSessionJSONL(t, root, dir, id, "to delete")
	// Add a same-named sidecar dir (blobs/sub-agents).
	encoded := encodeProjectDir(dir)
	sidecar := filepath.Join(root, "projects", encoded, id)
	if err := os.MkdirAll(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecar, "blob.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSession(context.Background(), dir, id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	jsonlPath := filepath.Join(root, "projects", encoded, id+".jsonl")
	if _, err := os.Stat(jsonlPath); !os.IsNotExist(err) {
		t.Errorf("jsonl still exists after delete: %v", err)
	}
	if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
		t.Errorf("sidecar still exists after delete: %v", err)
	}
}

func TestDeleteSession_MissingFileErrors(t *testing.T) {
	root := t.TempDir()
	c := newSessionsClient(t, root)
	id := "22222222-2222-2222-2222-222222222222"
	err := c.DeleteSession(context.Background(), "/home/user/p3", id)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("DeleteSession missing = %v, want a 'not found' error", err)
	}
}

func TestDeleteSession_DoesNotTouchMemoryDir(t *testing.T) {
	root := t.TempDir()
	dir := "/home/user/p4"
	c := newSessionsClient(t, root)
	id := "33333333-3333-3333-3333-333333333333"
	writeSessionJSONL(t, root, dir, id, "x")
	encoded := encodeProjectDir(dir)
	// Project-level shared memory dir (must NEVER be deleted).
	memDir := filepath.Join(root, "projects", encoded, "memory")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memDir, "CLAUDE.md"), []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteSession(context.Background(), dir, id); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := os.Stat(memDir); err != nil {
		t.Errorf("shared memory/ must survive session delete: %v", err)
	}
}

func TestReadSessionTitle_TruncatesLongPrompt(t *testing.T) {
	root := t.TempDir()
	long := strings.Repeat("中", maxSessionTitleRunes+10)
	path := filepath.Join(root, "s.jsonl")
	writeFile(t, root, "s.jsonl", `{"type":"queue-operation","content":"`+long+`"}`+"\n")
	got := readSessionTitle(path)
	wantRunes := maxSessionTitleRunes + len([]rune("…"))
	if len([]rune(got)) != wantRunes {
		t.Errorf("title rune count = %d, want %d (capped + ellipsis): %q", len([]rune(got)), wantRunes, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("title should end with ellipsis: %q", got)
	}
}

func TestReadSessionTitle_FallsBackToUserRecord(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "s.jsonl")
	// No queue-operation line; the first user record carries the content.
	body := `{"type":"system","subtype":"init","sessionId":"x"}` + "\n" +
		`{"type":"user","content":"hello world"}` + "\n"
	writeFile(t, root, "s.jsonl", body)
	got := readSessionTitle(path)
	if got != "hello world" {
		t.Errorf("title = %q, want %q", got, "hello world")
	}
}

func TestReadSessionTitle_UnnamedWhenEmpty(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "s.jsonl")
	// Only assistant/system lines — no usable title.
	body := `{"type":"assistant","content":[{"type":"text","text":"hi"}]}` + "\n"
	writeFile(t, root, "s.jsonl", body)
	if got := readSessionTitle(path); got != "" {
		t.Errorf("title = %q, want empty (caller renders unnamed)", got)
	}
}

// futureTime returns a time clearly after "now" so Chtimes moves mtime forward
// regardless of FS timestamp resolution.
func futureTime() (atime, mtime time.Time) {
	t := time.Unix(4_000_000_000, 0)
	return t, t
}
