package opencode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// TestBuildCommand_IncludesSessionAndModel verifies session/model/agent flags
// and the positional prompt are assembled correctly.
func TestBuildCommand_IncludesSessionAndModel(t *testing.T) {
	c := New(Config{CLIPath: "opencode"}, log.Nop())
	cmd, err := c.buildCommand(context.Background(), RunOptions{
		Prompt:    "hi",
		SessionID: "sess-1",
		Model:     "anthropic/claude",
		Agent:     "build",
	})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if !slices.Contains(cmd.Args, "--session") {
		t.Errorf("missing --session, args=%v", cmd.Args)
	}
	if !slices.Contains(cmd.Args, "--model") {
		t.Errorf("missing --model, args=%v", cmd.Args)
	}
	if !slices.Contains(cmd.Args, "--agent") {
		t.Errorf("missing --agent, args=%v", cmd.Args)
	}
	// Prompt is the last positional arg.
	if cmd.Args[len(cmd.Args)-1] != "hi" {
		t.Errorf("last arg = %q, want prompt", cmd.Args[len(cmd.Args)-1])
	}
}

// TestBuildCommand_OmitsEmptyFlags verifies session/model/agent are absent when
// unset so the CLI uses its own defaults.
func TestBuildCommand_OmitsEmptyFlags(t *testing.T) {
	c := New(Config{CLIPath: "opencode"}, log.Nop())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	for _, flag := range []string{"--session", "--model", "--agent"} {
		if slices.Contains(cmd.Args, flag) {
			t.Errorf("did not expect %s when unset, args=%v", flag, cmd.Args)
		}
	}
}

// TestBuildCommand_SetsProcessGroup verifies the CLI runs as its own process
// group leader, so cancellation can SIGKILL the whole tree.
func TestBuildCommand_SetsProcessGroup(t *testing.T) {
	c := New(Config{CLIPath: "opencode"}, log.Nop())
	cmd, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"})
	if err != nil {
		t.Fatalf("buildCommand: %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("expected cmd.SysProcAttr.Setpgid == true so the process group is killable on cancel")
	}
}

// TestBuildCommand_EmptyCLIPathErrors verifies a missing CLI path fails fast.
func TestBuildCommand_EmptyCLIPathErrors(t *testing.T) {
	c := New(Config{}, log.Nop())
	if _, err := c.buildCommand(context.Background(), RunOptions{Prompt: "hi"}); err == nil {
		t.Fatal("expected error for empty cli_path")
	}
}

// TestNew_ConcurrencyDefault verifies MaxConcurrent<=0 falls back to the default.
func TestNew_ConcurrencyDefault(t *testing.T) {
	c := New(Config{}, log.Nop())
	if cap(c.sem) != defaultMaxConcurrent {
		t.Errorf("sem cap = %d, want default %d", cap(c.sem), defaultMaxConcurrent)
	}
	c2 := New(Config{MaxConcurrent: 2}, log.Nop())
	if cap(c2.sem) != 2 {
		t.Errorf("sem cap = %d, want 2", cap(c2.sem))
	}
}

// writeFakeOpencode creates a fake `opencode` shell script that prints the
// given stdout when invoked with the matching subcommand. It returns the path
// to the script. Tests use this instead of the real CLI so ListModels /
// ListAgents are exercised against realistic output without a network.
func writeFakeOpencode(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake binary test relies on a POSIX shell")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	// The script shebang is /bin/sh; platforms without it skip via the GOOS
	// guard above.
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return path
}

// TestListModels_ParsesProviderModelLines verifies ListModels returns each
// non-empty stdout line trimmed, matching the real `opencode models` output
// shape (one `provider/model` per line).
func TestListModels_ParsesProviderModelLines(t *testing.T) {
	path := writeFakeOpencode(t, `case "$1" in
  models) printf 'opencode/big-pickle\nzhipuai-coding-plan/glm-5.2\n\n  kimi-for-coding/k2\n' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: path}, log.Nop())
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	want := []string{"opencode/big-pickle", "zhipuai-coding-plan/glm-5.2", "kimi-for-coding/k2"}
	if !slices.Equal(got, want) {
		t.Errorf("ListModels = %v, want %v", got, want)
	}
}

// TestListAgents_FiltersHidden verifies ListAgents parses `name (role)` lines
// and drops opencode's hidden internal agents (compaction/summary/title).
func TestListAgents_FiltersHidden(t *testing.T) {
	// Shape mirrors real `opencode agent list`: visible agents plus hidden
	// internal ones, each followed by an indented permissions JSON block.
	path := writeFakeOpencode(t, `case "$1 $2" in
  "agent list") printf 'build (primary)\n  [ {"permission":"*"} ]\nexplore (subagent)\n  [ {} ]\ncompaction (primary)\n  [ {} ]\nsummary (primary)\n  [ {} ]\ntitle (primary)\n  [ {} ]\ngeneral (subagent)\n  [ {} ]\nplan (primary)\n  [ {} ]\n' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: path}, log.Nop())
	got, err := c.ListAgents(context.Background())
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	// Permissions lines start with "  [" so they survive trimming; ListAgents
	// is expected to return agent name lines only (those containing " ("),
	// not the bracketed permission blocks.
	for _, a := range got {
		if strings.HasPrefix(a, "[") {
			t.Errorf("permission block leaked into agent list: %q", a)
		}
	}
	for _, hidden := range []string{"compaction", "summary", "title"} {
		if slices.Contains(got, hidden) {
			t.Errorf("hidden agent %q should be filtered, got %v", hidden, got)
		}
	}
	for _, visible := range []string{"build", "explore", "general", "plan"} {
		if !slices.Contains(got, visible) {
			t.Errorf("visible agent %q missing, got %v", visible, got)
		}
	}
}

// TestExecLines_EmptyCLIPathErrors verifies the guard matches buildCommand's
// behaviour so a misconfigured backend fails fast on /model too.
func TestExecLines_EmptyCLIPathErrors(t *testing.T) {
	c := New(Config{}, log.Nop())
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("expected error for empty cli_path")
	}
}

// TestExecLines_SubprocessFailure propagates a non-zero exit as an error so a
// broken CLI install surfaces to the user instead of an empty model list.
func TestExecLines_SubprocessFailure(t *testing.T) {
	path := writeFakeOpencode(t, `echo "boom" >&2; exit 2`)
	c := New(Config{CLIPath: path}, log.Nop())
	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("expected error from failing subprocess")
	}
	if !strings.Contains(err.Error(), "opencode models") {
		t.Errorf("error should name the subcommand, got: %v", err)
	}
}

// TestExecLines_DoesNotBlockSem verifies list queries skip the Run semaphore:
// saturating c.sem must not prevent a concurrent ListModels. A Run slot held
// open for the whole test would deadlock ListModels if it acquired the sem.
func TestExecLines_DoesNotBlockSem(t *testing.T) {
	path := writeFakeOpencode(t, `case "$1" in
  models) echo "p/m" ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`)
	c := New(Config{CLIPath: path, MaxConcurrent: 1}, log.Nop())
	// Hold the single Run slot for the test's duration.
	c.sem <- struct{}{}
	t.Cleanup(func() { <-c.sem })
	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels should not block on sem: %v", err)
	}
	if len(got) != 1 || got[0] != "p/m" {
		t.Errorf("ListModels = %v, want [p/m]", got)
	}
}

// countingFakeOpencode writes a fake `opencode` that appends a line to a count
// file on every invocation of the `models` subcommand. Returns the binary
// path and the count-file path so a test can assert how many times the CLI
// was actually forked (cache hit = no increment).
func countingFakeOpencode(t *testing.T) (binPath, countPath string) {
	t.Helper()
	dir := t.TempDir()
	binPath = filepath.Join(dir, "opencode")
	countPath = filepath.Join(dir, "calls")
	// Each `opencode models` call appends a line to the count file. The count
	// file thus records the true number of subprocess forks.
	script := `case "$1" in
  models) echo "call" >> "` + countPath + `"; printf 'p/a\np/b\n' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac`
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake opencode: %v", err)
	}
	return binPath, countPath
}

// callCount reads the count file written by countingFakeOpencode.
func callCount(t *testing.T, countPath string) int {
	t.Helper()
	data, err := os.ReadFile(countPath)
	if err != nil {
		return 0 // file not yet created = zero calls
	}
	return strings.Count(string(data), "\n")
}

// TestListModels_CacheHitsWithinTTL verifies that a second ListModels within
// the TTL does NOT fork the CLI again (count file stays at 1).
func TestListModels_CacheHitsWithinTTL(t *testing.T) {
	bin, count := countingFakeOpencode(t)
	c := New(Config{CLIPath: bin, ListCacheTTL: 3600}, log.Nop())

	first, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("first ListModels: %v", err)
	}
	if callCount(t, count) != 1 {
		t.Fatalf("expected 1 fork after first call, got %d", callCount(t, count))
	}

	second, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("second ListModels: %v", err)
	}
	if callCount(t, count) != 1 {
		t.Errorf("cache should hit (no new fork), got %d forks", callCount(t, count))
	}
	if !slices.Equal(first, second) {
		t.Errorf("cached result changed: first=%v second=%v", first, second)
	}
}

// TestListModels_CacheExpiresAfterTTL verifies that after the TTL elapses the
// next call forks the CLI again (count goes to 2).
func TestListModels_CacheExpiresAfterTTL(t *testing.T) {
	bin, count := countingFakeOpencode(t)
	// 1-second TTL so the test can wait it out cheaply.
	c := New(Config{CLIPath: bin, ListCacheTTL: 1}, log.Nop())

	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if callCount(t, count) != 1 {
		t.Fatalf("expected 1 fork, got %d", callCount(t, count))
	}

	time.Sleep(1100 * time.Millisecond)

	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("post-TTL call: %v", err)
	}
	if callCount(t, count) != 2 {
		t.Errorf("expected 2 forks after TTL expiry, got %d", callCount(t, count))
	}
}

// TestListModels_CacheDisabledWhenTTLZeroOrNegative verifies that ListCacheTTL
// <= 0 turns caching off: every call forks the CLI.
func TestListModels_CacheDisabledWhenTTLZeroOrNegative(t *testing.T) {
	for _, ttl := range []int{0, -1} {
		t.Run(fmt.Sprintf("ttl=%d", ttl), func(t *testing.T) {
			bin, count := countingFakeOpencode(t)
			c := New(Config{CLIPath: bin, ListCacheTTL: ttl}, log.Nop())
			for i := 1; i <= 2; i++ {
				if _, err := c.ListModels(context.Background()); err != nil {
					t.Fatalf("call %d: %v", i, err)
				}
				if got := callCount(t, count); got != i {
					t.Errorf("after %d calls with caching off, want %d forks, got %d", i, i, got)
				}
			}
		})
	}
}

// TestCachedList_ReturnedSliceIsACopy verifies the caller cannot mutate the
// cached values (the cache stores a defensive copy).
func TestCachedList_ReturnedSliceIsACopy(t *testing.T) {
	bin, _ := countingFakeOpencode(t)
	c := New(Config{CLIPath: bin, ListCacheTTL: 3600}, log.Nop())

	got, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	got[0] = "MUTATED"

	again, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("second ListModels: %v", err)
	}
	if again[0] == "MUTATED" {
		t.Error("caller mutation leaked into cache; cache must store a copy")
	}
}
