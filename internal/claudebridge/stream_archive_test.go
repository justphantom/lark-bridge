package claudebridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// newArchiveHandler builds a Handler whose only relevant state for the
// archive is stateDir + streamHistory; router/api/rpc are nil-stubbed since
// newStreamSink touches neither.
func newArchiveHandler(t *testing.T, streamHistory int) *Handler {
	t.Helper()
	return &Handler{
		stateDir:      t.TempDir(),
		streamHistory: streamHistory,
		logger:        log.Nop(),
	}
}

// TestNewStreamSink_DisabledWhenZero locks in that <=0 streamHistory yields no
// sink: the feature is opt-out and a disabled handler must not touch the FS.
func TestNewStreamSink_DisabledWhenZero(t *testing.T) {
	h := newArchiveHandler(t, 0)
	sink, closeSink := h.newStreamSink("chat-1", "msg-1")
	if sink != nil || closeSink != nil {
		t.Fatal("expected nil sink when streamHistory<=0")
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, streamSubdir)); !os.IsNotExist(err) {
		t.Fatalf("streams dir should not be created when disabled, got err=%v", err)
	}
}

// TestNewStreamSink_WritesAndPersists proves the returned sink lands bytes in
// a file under {stateDir}/streams and that closing flushes the handle.
func TestNewStreamSink_WritesAndPersists(t *testing.T) {
	h := newArchiveHandler(t, 50)
	sink, closeSink := h.newStreamSink("chat-1", "msg-1")
	if sink == nil || closeSink == nil {
		t.Fatal("expected non-nil sink")
	}
	if _, err := sink.Write([]byte("{\"a\":1}\n{\"b\":2}\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := closeSink(); err != nil {
		t.Fatalf("close: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(h.stateDir, streamSubdir))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	got, err := os.ReadFile(filepath.Join(h.stateDir, streamSubdir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != "{\"a\":1}\n{\"b\":2}\n" {
		t.Fatalf("unexpected content %q", string(got))
	}
	if !strings.Contains(entries[0].Name(), "chat-1") ||
		!strings.Contains(entries[0].Name(), "msg-1") ||
		!strings.HasSuffix(entries[0].Name(), ".jsonl") {
		t.Fatalf("unexpected filename %q", entries[0].Name())
	}
}

// TestWrapThinkingFilter_DropsThinkingTokensLines locks the scheme-3a archive
// slimming: thinking_tokens dominates claude archives (~88% of lines, never
// consumed by the bridge) so the wrapped sink drops those lines while keeping
// every other kind intact. pump writes one line per Write call, so the filter
// classifies whole lines without splitting.
func TestWrapThinkingFilter_DropsThinkingTokensLines(t *testing.T) {
	h := newArchiveHandler(t, 50)
	sink, closeSink := h.newStreamSink("chat-1", "msg-1")
	if sink == nil {
		t.Fatal("expected non-nil sink")
	}
	wrapped := wrapThinkingFilter(sink)
	// Mix of real line kinds (实测 023537: thinking_tokens + init + assistant).
	lines := []string{
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":1024,"session_id":"s1"}`,
		`{"type":"system","subtype":"init","session_id":"s1","model":"claude-sonnet-5"}`,
		`{"type":"system","subtype":"thinking_tokens","estimated_tokens":2048,"session_id":"s1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"system","subtype":"task_updated","task_id":"t1"}`,
	}
	for _, line := range lines {
		if _, err := wrapped.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := closeSink(); err != nil {
		t.Fatalf("close: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(h.stateDir, streamSubdir))
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(h.stateDir, streamSubdir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	body := string(got)
	if strings.Contains(body, "thinking_tokens") {
		t.Errorf("thinking_tokens lines must be dropped, got: %s", body)
	}
	// Non-thinking lines must survive.
	for _, keep := range []string{`"subtype":"init"`, `"type":"assistant"`, "task_updated"} {
		if !strings.Contains(body, keep) {
			t.Errorf("non-thinking line %q must survive the filter, got: %s", keep, body)
		}
	}
	// wrapThinkingFilter(nil) returns nil so callers can chain unconditionally.
	if wrapThinkingFilter(nil) != nil {
		t.Errorf("wrapThinkingFilter(nil) should return nil")
	}
}

// TestPruneStreamDir_KeepsNewest verifies rotation deletes oldest files and
// keeps exactly `keep` newest, using filename chronological order.
func TestPruneStreamDir_KeepsNewest(t *testing.T) {
	dir := t.TempDir()
	// Timestamp-prefixed names so lexical sort == chronological.
	names := []string{
		"20260101T000000.000000000_a_x.jsonl",
		"20260102T000000.000000000_a_x.jsonl",
		"20260103T000000.000000000_a_x.jsonl",
		"20260104T000000.000000000_a_x.jsonl",
		"20260105T000000.000000000_a_x.jsonl",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	// Non-.jsonl entries must be ignored by the prune.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write notes: %v", err)
	}

	pruneStreamDir(log.Nop(), dir, 2)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var got []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			got = append(got, e.Name())
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 .jsonl remaining, got %d (%v)", len(got), got)
	}
	if got[0] != "20260104T000000.000000000_a_x.jsonl" ||
		got[1] != "20260105T000000.000000000_a_x.jsonl" {
		t.Fatalf("expected the two newest to remain, got %v", got)
	}
}

// TestPruneStreamDir_NoopWhenUnderCap ensures prune leaves the directory
// untouched when the count is already within the cap.
func TestPruneStreamDir_NoopWhenUnderCap(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"20260101T000000.000000000_a_x.jsonl", "20260102T000000.000000000_a_x.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	pruneStreamDir(log.Nop(), dir, 5)
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("expected no deletion, got %d entries", len(entries))
	}
}

// TestSanitizeName locks the filename-safety contract: only [A-Za-z0-9._-]
// pass through, everything else collapses to '_', empty becomes "x", and a
// path-traversal segment is neutralised.
func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"oc_abc-123":       "oc_abc-123",
		"om/a/b":           "om_a_b",
		"../../etc/passwd": ".._.._etc_passwd",
		"":                 "x",
		"café":             "caf_",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewStreamSink_RotatesToCap drives the full newStreamSink path N+2 times
// and asserts the directory never exceeds the configured cap (the newest N
// survive, older ones are pruned).
func TestNewStreamSink_RotatesToCap(t *testing.T) {
	const cap = 3
	h := newArchiveHandler(t, cap)
	for i := 0; i < cap+2; i++ {
		sink, closeSink := h.newStreamSink("chat", "msg")
		if sink == nil {
			t.Fatalf("iter %d: nil sink", i)
		}
		// Distinct content so collisions on the same chat/msg id still write
		// separate lines if names ever collide.
		_, _ = sink.Write([]byte("x"))
		_ = closeSink()
		// Names embed nanosecond timestamps; force the next iteration's
		// timestamp to differ so each run lands its own file. Without this
		// two iterations can share a nanosecond (O_APPEND folds them into
		// one file), leaving fewer files than the cap and failing the count.
		time.Sleep(time.Millisecond)
	}
	entries, _ := os.ReadDir(filepath.Join(h.stateDir, streamSubdir))
	var n int
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			n++
		}
	}
	if n != cap {
		t.Fatalf("expected %d archived files after rotation, got %d", cap, n)
	}
}
