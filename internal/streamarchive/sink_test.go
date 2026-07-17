package streamarchive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hu/lark-bridge/internal/log"
)

// TestSanitizeName covers the filename-safety collapse: safe runes pass
// through, unsafe runes become '_', and empty input yields a placeholder.
func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"chat-123":   "chat-123",
		"oc_abc.def": "oc_abc.def",
		"chat 1":     "chat_1",
		"群/组":        "___",
		"":           "x",
		"../escape":  ".._escape",
		"a:b@c":      "a_b_c",
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNewSink_Disabled verifies history<=0 and empty stateDir both yield nil.
func TestNewSink_Disabled(t *testing.T) {
	dir := t.TempDir()
	lg := log.Nop()
	if w, c := NewSink(lg, dir, "claude", "c", "r", 0); w != nil || c != nil {
		t.Error("history=0 should disable")
	}
	if w, c := NewSink(lg, dir, "claude", "c", "r", -1); w != nil || c != nil {
		t.Error("history<0 should disable")
	}
	if w, c := NewSink(lg, "", "claude", "c", "r", 50); w != nil || c != nil {
		t.Error("empty stateDir should disable")
	}
}

// TestNewSink_CreatesPerBackendDir verifies the archive lands under
// streams/<backend>/ and writes reach the file.
func TestNewSink_CreatesPerBackendDir(t *testing.T) {
	stateDir := t.TempDir()
	lg := log.Nop()
	w, closeSink := NewSink(lg, stateDir, "claude", "chat-1", "reply-9", 50)
	if w == nil || closeSink == nil {
		t.Fatal("NewSink returned nil with archiving enabled")
	}
	if _, err := w.Write([]byte(`{"type":"text","content":"hi"}` + "\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := closeSink(); err != nil {
		t.Fatalf("closeSink: %v", err)
	}

	// File must be under streams/claude/, named with chat + reply segments.
	backendDir := filepath.Join(stateDir, "streams", "claude")
	entries, err := os.ReadDir(backendDir)
	if err != nil {
		t.Fatalf("ReadDir claude: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.Contains(name, "chat-1") || !strings.Contains(name, "reply-9") {
		t.Errorf("filename %q missing chat/reply segments", name)
	}
}

// TestNewSink_BackendIsolation verifies two backends archive into separate
// directories so a burst in one does not land in the other's dir.
func TestNewSink_BackendIsolation(t *testing.T) {
	stateDir := t.TempDir()
	lg := log.Nop()
	for _, b := range []string{"claude", "opencode"} {
		w, c := NewSink(lg, stateDir, b, "c", "r", 50)
		if w == nil {
			t.Fatalf("NewSink %s nil", b)
		}
		_, _ = w.Write([]byte("{}\n"))
		_ = c()
	}
	for _, b := range []string{"claude", "opencode"} {
		entries, err := os.ReadDir(filepath.Join(stateDir, "streams", b))
		if err != nil {
			t.Errorf("backend %s dir missing: %v", b, err)
			continue
		}
		if len(entries) != 1 {
			t.Errorf("backend %s: expected 1 file, got %d", b, len(entries))
		}
	}
}

// TestPrune verifies oldest files are deleted to keep the cap, and that prune
// only touches the given directory (isolation).
func TestPrune(t *testing.T) {
	lg := log.Nop()
	dir := t.TempDir()
	// Create 5 files with lexicographically increasing names.
	for _, n := range []string{"20260101T000000_a.jsonl", "20260102T000000_b.jsonl",
		"20260103T000000_c.jsonl", "20260104T000000_d.jsonl", "20260105T000000_e.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Non-jsonl files must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	Prune(lg, dir, 2)
	entries, _ := os.ReadDir(dir)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	// Keep newest 2 (d, e); note.txt untouched.
	if len(got) != 3 {
		t.Fatalf("after prune keep=2: got %v, want 3 entries (2 jsonl + note.txt)", got)
	}
	for _, mustKeep := range []string{"20260104T000000_d.jsonl", "20260105T000000_e.jsonl", "note.txt"} {
		found := false
		for _, g := range got {
			if g == mustKeep {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("prune deleted %q (should keep)", mustKeep)
		}
	}
}

// TestPrune_NewSinkIntegration verifies the cap is enforced through NewSink's
// built-in prune: writing more than history files via NewSink keeps only the
// newest history.
func TestPrune_NewSinkIntegration(t *testing.T) {
	stateDir := t.TempDir()
	lg := log.Nop()
	// history=3 → each NewSink prunes to keep 2 before adding 1 (net 3 cap).
	for i := 0; i < 6; i++ {
		w, c := NewSink(lg, stateDir, "claude", "c", "r", 3)
		if w == nil {
			t.Fatal("nil sink")
		}
		_, _ = w.Write([]byte("{}\n"))
		_ = c()
	}
	backendDir := filepath.Join(stateDir, "streams", "claude")
	entries, _ := os.ReadDir(backendDir)
	if len(entries) > 3 {
		t.Errorf("expected ≤3 files after 6 writes with history=3, got %d", len(entries))
	}
}
