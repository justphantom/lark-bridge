package opencode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/log"
)

// fakeOpencodeInDir writes a fake `opencode` binary to a temp dir and returns
// its path plus a fresh working dir. Tests pass the working dir to the
// client; the script can write marker files relative to cwd (which the
// client sets to the working dir) so assertions don't need the path up front.
func fakeOpencodeInDir(t *testing.T, script string) (bin, work string) {
	t.Helper()
	binDir := t.TempDir()
	work = t.TempDir()
	bin = filepath.Join(binDir, "opencode")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return bin, work
}

// TestListSessions_ParsesJSONArray verifies ListSessions decodes the CLI's
// `session list --format json` array into Session structs.
func TestListSessions_ParsesJSONArray(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `case "$1 $2" in
  "session list") printf '[{"id":"ses_a","title":"T-A","updated":1000,"created":500},{"id":"ses_b","title":"T-B","updated":2000,"created":1500}]' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: bin}, log.Nop())
	got, err := c.ListSessions(context.Background(), work)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(got), got)
	}
	if got[0].ID != "ses_a" || got[0].Title != "T-A" || got[0].Updated != 1000 || got[0].Created != 500 {
		t.Errorf("first session = %+v", got[0])
	}
	if got[1].ID != "ses_b" || got[1].Title != "T-B" {
		t.Errorf("second session = %+v", got[1])
	}
}

// TestListSessions_SetsCmdDir verifies the dir argument lands on cmd.Dir so
// the CLI enumerates the binding's directory and not the process cwd.
func TestListSessions_SetsCmdDir(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `case "$1 $2" in
  "session list")
    printf '[]'
    echo "$PWD" > cwd_marker
    ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: bin}, log.Nop())
	if _, err := c.ListSessions(context.Background(), work); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	marker, err := os.ReadFile(filepath.Join(work, "cwd_marker"))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.TrimSpace(string(marker)) != work {
		t.Errorf("cmd.Dir = %q, want %q", strings.TrimSpace(string(marker)), work)
	}
}

// TestListSessions_EmptyOutputReturnsNil verifies an empty stdout (e.g.
// `--max-count 0` or a fresh project) yields nil without error.
func TestListSessions_EmptyOutputReturnsNil(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `case "$1 $2" in
  "session list") printf '' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: bin}, log.Nop())
	got, err := c.ListSessions(context.Background(), work)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got != nil {
		t.Errorf("got %v, want nil for empty output", got)
	}
}

// TestListSessions_EmptyCLIPathErrors verifies the guard matches other client
// methods so a misconfigured backend fails fast on /session-list too.
func TestListSessions_EmptyCLIPathErrors(t *testing.T) {
	c := New(Config{}, log.Nop())
	if _, err := c.ListSessions(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty cli_path")
	}
}

// TestListSessions_MalformedJSONErrors verifies a non-JSON stdout surfaces as
// a parse error rather than an empty list.
func TestListSessions_MalformedJSONErrors(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `case "$1 $2" in
  "session list") printf 'not json' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: bin}, log.Nop())
	_, err := c.ListSessions(context.Background(), work)
	if err == nil {
		t.Fatal("expected parse error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "parse session list") {
		t.Errorf("error should name the parse step, got: %v", err)
	}
}

// TestListSessions_SubprocessFailure verifies a non-zero exit propagates.
func TestListSessions_SubprocessFailure(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `echo "boom" >&2; exit 2`)
	c := New(Config{CLIPath: bin}, log.Nop())
	_, err := c.ListSessions(context.Background(), work)
	if err == nil {
		t.Fatal("expected error from failing subprocess")
	}
	if !strings.Contains(err.Error(), "opencode session list") {
		t.Errorf("error should name the subcommand, got: %v", err)
	}
}

// TestDeleteSession_PassesArgsAndDir verifies DeleteSession invokes
// `session delete <id>` with cmd.Dir set so the CLI looks up the session in
// the binding's project.
func TestDeleteSession_PassesArgsAndDir(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `case "$1 $2" in
  "session delete")
    printf '%s\n' "$3" > argv_marker
    printf '%s\n' "$PWD" > pwd_marker
    ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: bin}, log.Nop())
	if err := c.DeleteSession(context.Background(), work, "ses_xyz"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	argv, _ := os.ReadFile(filepath.Join(work, "argv_marker"))
	if strings.TrimSpace(string(argv)) != "ses_xyz" {
		t.Errorf("argv = %q, want ses_xyz", strings.TrimSpace(string(argv)))
	}
	pwd, _ := os.ReadFile(filepath.Join(work, "pwd_marker"))
	if strings.TrimSpace(string(pwd)) != work {
		t.Errorf("cmd.Dir = %q, want %q", strings.TrimSpace(string(pwd)), work)
	}
}

// TestDeleteSession_SubprocessFailureIncludesOutput verifies the error wraps
// both the exit error and the CLI's combined output so "Session not found"
// surfaces verbatim.
func TestDeleteSession_SubprocessFailureIncludesOutput(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `echo "Error: Session not found: ses_x" >&2; exit 1`)
	c := New(Config{CLIPath: bin}, log.Nop())
	err := c.DeleteSession(context.Background(), work, "ses_x")
	if err == nil {
		t.Fatal("expected error from failing subprocess")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ses_x") {
		t.Errorf("error should include the session id: %v", err)
	}
	if !strings.Contains(msg, "Session not found") {
		t.Errorf("error should include the CLI's verbatim output: %v", err)
	}
}

// TestDeleteSession_EmptyCLIPathErrors verifies the guard matches other
// methods.
func TestDeleteSession_EmptyCLIPathErrors(t *testing.T) {
	c := New(Config{}, log.Nop())
	if err := c.DeleteSession(context.Background(), "", "ses_x"); err == nil {
		t.Fatal("expected error for empty cli_path")
	}
}

// TestDeleteSession_EmptySessionIDErrors guards against a caller bug that
// would otherwise fork `opencode session delete` with no positional arg
// (which prints help and exits 1).
func TestDeleteSession_EmptySessionIDErrors(t *testing.T) {
	bin, work := fakeOpencodeInDir(t, `echo "should not be called" >&2; exit 1`)
	c := New(Config{CLIPath: bin}, log.Nop())
	if err := c.DeleteSession(context.Background(), work, ""); err == nil {
		t.Fatal("expected error for empty session id")
	}
}
