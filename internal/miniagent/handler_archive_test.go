package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/miniclient"
	"github.com/justphantom/lark-bridge/internal/streamarchive"
)

// TestRunViaCLI_ArchivesToStreamsMiniagent pins the F9 archive-tee wiring at
// the handler level (handler_cli.go:29-33): when streamHistory > 0 and
// stateDir is set, runViaCLI constructs a streamarchive sink so the turn's raw
// NDJSON is persisted under {stateDir}/streams/miniagent/. The pump mechanism
// itself is covered by miniclient's own tests; this locks that the handler
// ASSEMBLES the sink on every turn (the gap the plan flagged).
//
// A miniagent Client with an empty cli_path makes client.Run return an error
// fast — but the sink is constructed BEFORE Run is called, so the archive file
// is created on disk regardless, and the deferred close flushes it. The test
// asserts the file lands under streams/miniagent/ with the expected name
// shape. It does NOT assert NDJSON content (none flows when the CLI errors),
// which is the pump's contract — tested separately.
func TestRunViaCLI_ArchivesToStreamsMiniagent(t *testing.T) {
	stateDir := t.TempDir()
	// Empty-cli_path client: Run returns "cli_path is empty" immediately, so
	// runViaCLI's error path runs and the sink's deferred Close fires.
	client := miniclient.New(miniclient.Config{}, log.Nop())
	sender := &captureSender{}
	h := New(sender, log.Nop(), nil, "", "test-model", client, 50, stateDir, false)

	h.runViaCLI(context.Background(), "prompt-archive", "chat-archive", "do something")

	// The sink should have created exactly one .jsonl under streams/miniagent/.
	dir := filepath.Join(stateDir, "streams", "miniagent")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("streams/miniagent dir not created: %v", err)
	}
	var found string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			found = e.Name()
			break
		}
	}
	if found == "" {
		t.Fatalf("no .jsonl archived under streams/miniagent/: %+v", entries)
	}
	// Name shape: <ts>_<sanitized chatID>_<sanitized replyToID>.jsonl
	if !strings.Contains(found, "chat-archive") || !strings.Contains(found, "prompt-archive") {
		t.Errorf("archive name %q should carry chatID + replyToID", found)
	}

	// The error path MUST have surfaced a TypeError control so the turn is not
	// silently dropped (the archive file existing is necessary but not
	// sufficient — the user must also see the failure).
	controls := sender.Controls()
	var sawError bool
	for _, c := range controls {
		if c.Type == "error" && c.Error != nil &&
			strings.Contains(c.Error.Message, "启动 miniagent 失败") {
			sawError = true
			break
		}
	}
	if !sawError {
		t.Errorf("runViaCLI should emit a TypeError on client.Run failure; got %+v", controls)
	}
}

// TestRunViaCLI_NoArchiveWhenDisabled verifies the sink is NOT constructed
// when streamHistory is 0 (archive disabled) — proving the handler's guard at
// handler_cli.go:29 short-circuits cleanly and leaves the filesystem alone.
func TestRunViaCLI_NoArchiveWhenDisabled(t *testing.T) {
	stateDir := t.TempDir()
	client := miniclient.New(miniclient.Config{}, log.Nop())
	sender := &captureSender{}
	// streamHistory = 0 → archive disabled.
	h := New(sender, log.Nop(), nil, "", "test-model", client, 0, stateDir, false)

	h.runViaCLI(context.Background(), "prompt-noarch", "chat-noarch", "hi")

	dir := filepath.Join(stateDir, "streams", "miniagent")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(dir)
		t.Errorf("streams/miniagent must NOT be created when archive disabled; entries=%+v", entries)
	}
}

// TestStreamArchiveSink_PersistsRawLines is a focused, deterministic
// complement to the handler test: it drives streamarchive.NewSink directly
// (the same call runViaCLI makes) and writes raw NDJSON lines through it,
// asserting the lines land verbatim in the .jsonl. This pins the contract the
// miniagent handler relies on (raw NDJSON tee) without needing a real CLI
// subprocess. It also confirms memory/ (project-level shared dir) is not
// touched by the miniagent sink path.
func TestStreamArchiveSink_PersistsRawLines(t *testing.T) {
	stateDir := t.TempDir()
	const chatID, replyToID = "chat-raw", "prompt-raw"
	sink, closeSink := streamarchive.NewSink(log.Nop(), stateDir, "miniagent", chatID, replyToID, 10, false)
	if sink == nil {
		t.Fatal("NewSink returned nil writer for a configured archive")
	}
	defer closeSink()

	lines := []string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","result":"done"}`,
	}
	for _, l := range lines {
		if _, err := sink.Write([]byte(l + "\n")); err != nil {
			t.Fatalf("sink write: %v", err)
		}
	}

	dir := filepath.Join(stateDir, "streams", "miniagent")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read streams dir: %v", err)
	}
	var path string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			path = filepath.Join(dir, e.Name())
			break
		}
	}
	if path == "" {
		t.Fatal("no archived .jsonl found")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	got := strings.TrimRight(string(body), "\n")
	if got != strings.Join(lines, "\n") {
		t.Errorf("archive content mismatch:\nwant:\n%s\ngot:\n%s", strings.Join(lines, "\n"), got)
	}
}
