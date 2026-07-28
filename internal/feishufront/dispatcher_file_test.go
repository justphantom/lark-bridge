package feishufront

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/fileconvert"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// testPromptTemplate is the canonical wording copied from config.example.json.
// Kept inline (rather than reached for via config.Load) so a config regression
// does not cascade into dispatcher tests; the config_validate tests cover the
// on-disk template.
const testPromptTemplate = `用户上传了一份文件并希望基于其内容进行处理。
文件名：{{.FileName}}
已转换为 Markdown，路径：{{.Path}}
请先用 Read 工具读取该文件，再按用户的进一步指示处理。{{if .UserText}}

用户的附加说明：{{.UserText}}{{end}}`

// mustParseTestTemplate parses the canonical test template, failing the test
// if it cannot parse. Used in every dispatcher file test so a syntax error
// surfaces immediately rather than as a nil-deref later.
func mustParseTestTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.New("test").Parse(testPromptTemplate)
	if err != nil {
		t.Fatalf("parse test template: %v", err)
	}
	return tmpl
}

// fakeDownloader serves a fixed byte body for any DownloadFile call.
type fakeDownloader struct {
	body  []byte
	err   error
	calls int
}

func (f *fakeDownloader) DownloadFile(_ context.Context, _, _, _ string) (io.ReadCloser, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(string(f.body))), nil
}

// wireFileDispatcher builds a fresh Dispatcher with a registered backend, a
// fake downloader carrying `body`, an in-memory converter, and a temp inbox
// at `inbox`. Returns the dispatcher, the backend's event channel, the
// downloader, and the sink so tests can inspect what the user saw.
func wireFileDispatcher(t *testing.T, body []byte, inbox string, maxSize int64) (*Dispatcher, *BackendConn, *fakeDownloader, *fakeSink) {
	t.Helper()
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	const backendID = "claude-1"
	reg.Register(backendID, "claude")
	conn, _ := reg.Get(backendID)
	dl := &fakeDownloader{body: body}
	d := NewDispatcher(sink, reg, NewTurnManager(), boundRouter{backendID: backendID})
	d.SetFilePipeline(dl, fileconvert.New(fileconvert.Options{}), inbox, maxSize, mustParseTestTemplate(t))
	return d, conn, dl, sink
}

// TestDispatchIncoming_FileRejectedWhenPipelineDisabled verifies the file
// pipeline is opt-in: with no SetFilePipeline wiring the dispatcher must
// reject file messages with the legacy notice, not crash, and never start a
// turn.
func TestDispatchIncoming_FileRejectedWhenPipelineDisabled(t *testing.T) {
	sink := &fakeSink{}
	d := NewDispatcher(sink, NewBackendRegistry(), NewTurnManager(), nil)
	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID: "evt_file", MessageID: "om_file", ChatID: "oc_chat",
		MsgType:  "file",
		FileKey:  "fk1",
		FileName: "a.md",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice, got %d", len(sink.sends))
	}
}

// TestDispatchIncoming_MarkdownUploadedAndForwarded verifies the happy path:
// a .md upload is downloaded, copied verbatim into the inbox, and a Prompt
// Event reaches the backend with the .md path embedded in the prompt text.
func TestDispatchIncoming_MarkdownUploadedAndForwarded(t *testing.T) {
	body := []byte("# title\n\nhello world\n")
	d, conn, dl, sink := wireFileDispatcher(t, body, t.TempDir(), 5<<20)

	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_file",
		MessageID: "om_file",
		ChatID:    "oc_chat",
		MsgType:   "file",
		FileKey:   "fk1",
		FileName:  "notes.md",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if dl.calls != 1 {
		t.Fatalf("downloader called %d times, want 1", dl.calls)
	}
	// The placeholder progress card SendCard is the first card the dispatcher
	// emits; assert it fired so we know the turn was started.
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 placeholder send, got %d", len(sink.sends))
	}

	select {
	case ev := <-conn.eventCh:
		if ev.Type != protocol.TypePrompt {
			t.Fatalf("want prompt event, got %q", ev.Type)
		}
		p := ev.Prompt
		if !strings.Contains(p.Text, "notes.md") {
			t.Errorf("prompt text missing file name: %q", p.Text)
		}
		// Locate the absolute path on its own line and verify the file
		// actually exists with the body bytes verbatim (md copy path).
		idx := strings.Index(p.Text, "路径：")
		if idx < 0 {
			t.Fatalf("prompt text missing path marker: %q", p.Text)
		}
		tail := p.Text[idx+len("路径："):]
		end := strings.IndexByte(tail, '\n')
		if end < 0 {
			end = len(tail)
		}
		abs := strings.TrimSpace(tail[:end])
		got, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("read converted file %s: %v", abs, err)
		}
		if string(got) != string(body) {
			t.Errorf("converted content = %q, want %q", got, body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no prompt event received")
	}
}

// TestDispatchIncoming_TextBodySizeEnforced verifies the MaxFileSize ceiling
// fires BEFORE the conversion runs: an oversized upload is removed and the
// user sees a "文件过大" notice instead of a turn.
func TestDispatchIncoming_TextBodySizeEnforced(t *testing.T) {
	body := make([]byte, 6<<20) // 6 MiB against a 5 MiB ceiling
	for i := range body {
		body[i] = 'x'
	}
	d, conn, _, sink := wireFileDispatcher(t, body, t.TempDir(), 5<<20)

	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_big",
		MessageID: "om_big",
		ChatID:    "oc_chat",
		MsgType:   "file",
		FileKey:   "fk1",
		FileName:  "huge.md",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice, got %d sends", len(sink.sends))
	}
	if !strings.Contains(string(sink.sends[0].card), "文件过大") {
		t.Errorf("notice card missing '文件过大': %s", sink.sends[0].card)
	}
	select {
	case ev := <-conn.eventCh:
		t.Fatalf("no prompt expected for oversized upload, got %+v", ev)
	default:
		// good
	}
}

// TestDispatchIncoming_UnsupportedExtension verifies a .pdf upload is rejected
// with a friendly notice before any download round-trip.
func TestDispatchIncoming_UnsupportedExtension(t *testing.T) {
	d, conn, dl, sink := wireFileDispatcher(t, []byte("x"), t.TempDir(), 5<<20)

	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_pdf",
		MessageID: "om_pdf",
		ChatID:    "oc_chat",
		MsgType:   "file",
		FileKey:   "fk1",
		FileName:  "doc.pdf",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice, got %d", len(sink.sends))
	}
	if !strings.Contains(string(sink.sends[0].card), "暂不支持的文件类型") {
		t.Errorf("notice card missing extension message: %s", sink.sends[0].card)
	}
	if dl.calls != 0 {
		t.Errorf("downloader should not be called for unsupported ext, got %d calls", dl.calls)
	}
	select {
	case ev := <-conn.eventCh:
		t.Fatalf("no prompt expected for unsupported ext, got %+v", ev)
	default:
		// good
	}
}

// TestDispatchIncoming_DownloadErrorNotice verifies a download failure
// surfaces as a user-facing "下载失败" notice instead of crashing or starting
// a turn.
func TestDispatchIncoming_DownloadErrorNotice(t *testing.T) {
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	const backendID = "claude-1"
	reg.Register(backendID, "claude")
	conn, _ := reg.Get(backendID)
	dl := &fakeDownloader{err: errFake()}
	d := NewDispatcher(sink, reg, NewTurnManager(), boundRouter{backendID: backendID})
	d.SetFilePipeline(dl, fileconvert.New(fileconvert.Options{}), t.TempDir(), 5<<20, mustParseTestTemplate(t))

	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_dl",
		MessageID: "om_dl",
		ChatID:    "oc_chat",
		MsgType:   "file",
		FileKey:   "fk1",
		FileName:  "ok.md",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice, got %d sends", len(sink.sends))
	}
	if !strings.Contains(string(sink.sends[0].card), "下载失败") {
		t.Errorf("notice card missing 下载失败: %s", sink.sends[0].card)
	}
	select {
	case ev := <-conn.eventCh:
		t.Fatalf("no prompt expected after download error, got %+v", ev)
	default:
		// good
	}
}

type fakeError struct{ msg string }

func (e *fakeError) Error() string { return e.msg }
func errFake() error               { return &fakeError{msg: "simulated 403"} }

// TestExecuteFilePromptTemplate_DefaultWording verifies the canonical
// template (the one shipped in config.example.json) renders the expected
// structured body: header, file name, path, instruction, and a conditional
// user-text line that disappears when the upload carried no body.
func TestExecuteFilePromptTemplate_DefaultWording(t *testing.T) {
	cases := []struct {
		name     string
		userText string
		wantHas  []string
		wantDrop []string
	}{
		{
			name:     "with user text",
			userText: "请总结要点",
			wantHas:  []string{"notes.md", "/abs/path/notes.md", "请先用 Read", "用户的附加说明：请总结要点"},
			wantDrop: nil,
		},
		{
			name:     "without user text",
			userText: "",
			wantHas:  []string{"notes.md", "请先用 Read"},
			wantDrop: []string{"用户的附加说明"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewDispatcher(&fakeSink{}, NewBackendRegistry(), NewTurnManager(), nil)
			d.SetFilePipeline(&fakeDownloader{}, fileconvert.New(fileconvert.Options{}), t.TempDir(), 5<<20, mustParseTestTemplate(t))

			msg := &feishu.IncomingMessage{Content: c.userText}
			got, err := d.executeFilePromptTemplate("notes.md", "/abs/path/notes.md", msg)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			for _, want := range c.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in rendered prompt:\n%s", want, got)
				}
			}
			for _, drop := range c.wantDrop {
				if strings.Contains(got, drop) {
					t.Errorf("expected %q to be omitted, but found in:\n%s", drop, got)
				}
			}
		})
	}
}

// TestExecuteFilePromptTemplate_CustomTemplate verifies the dispatcher
// honours an operator-supplied template verbatim — including non-default
// wording, English text, and alternative variable order. This pins the
// "code never ships a compiled-in template" contract.
func TestExecuteFilePromptTemplate_CustomTemplate(t *testing.T) {
	custom := `Read {{.Path}} (uploaded as {{.FileName}}) and follow the user's instructions.`
	tmpl, err := template.New("custom").Parse(custom)
	if err != nil {
		t.Fatalf("parse custom: %v", err)
	}
	d := NewDispatcher(&fakeSink{}, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetFilePipeline(&fakeDownloader{}, fileconvert.New(fileconvert.Options{}), t.TempDir(), 5<<20, tmpl)

	got, err := d.executeFilePromptTemplate("report.docx", "/inbox/oc/p/report.md", &feishu.IncomingMessage{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := "Read /inbox/oc/p/report.md (uploaded as report.docx) and follow the user's instructions."
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestExecuteFilePromptTemplate_BadTemplateFailsGracefully verifies a
// template that parses but fails at execute (referencing a missing field)
// surfaces as an error the dispatcher will turn into a user-facing notice
// rather than a panic. Goes through DispatchIncoming end-to-end so the
// notice path is exercised too.
func TestExecuteFilePromptTemplate_BadTemplateFailsGracefully(t *testing.T) {
	// {{.Missing}} references an undefined struct field → execute error.
	tmpl, err := template.New("bad").Parse("path={{.Missing}}")
	if err != nil {
		t.Fatalf("parse: %v — unexpected, Missing is a runtime not parse error", err)
	}
	d, conn, _, sink := wireFileDispatcher(t, []byte("body"), t.TempDir(), 5<<20)
	d.promptTemplate = tmpl // override with the broken one

	err = d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_bad",
		MessageID: "om_bad",
		ChatID:    "oc_chat",
		MsgType:   "file",
		FileKey:   "fk1",
		FileName:  "x.md",
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice, got %d", len(sink.sends))
	}
	if !strings.Contains(string(sink.sends[0].card), "渲染失败") {
		t.Errorf("expected 渲染失败 notice, got: %s", sink.sends[0].card)
	}
	select {
	case ev := <-conn.eventCh:
		t.Fatalf("no prompt expected on template failure, got %+v", ev)
	default:
	}
}

// TestSanitizePathElement verifies the inbox path element reject-list: only
// alnum/-/_/. survive, and any ".." is collapsed.
func TestSanitizePathElement(t *testing.T) {
	cases := []struct{ in, want string }{
		{"oc_abc123", "oc_abc123"},
		{"om_123", "om_123"},
		// "../etc/passwd" — '/' is not in the allowlist so each becomes '_';
		// the leading ".." then collapses to one '_', joining with the next
		// underscore from the first '/'. Net: no traversal chars survive.
		{"../etc/passwd", "__etc_passwd"},
		{"", "unknown"},
		{"a..b", "a_b"},
		{"with space", "with_space"},
	}
	for _, c := range cases {
		if got := sanitizePathElement(c.in); got != c.want {
			t.Errorf("sanitizePathElement(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseFileNameFromContent verifies the fallback parser surfaces
// file_name when the parsed field is empty.
func TestParseFileNameFromContent(t *testing.T) {
	if got := parseFileNameFromContent(`{"file_key":"fk","file_name":"report.docx"}`); got != "report.docx" {
		t.Errorf("got %q, want report.docx", got)
	}
	if got := parseFileNameFromContent(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	if got := parseFileNameFromContent("not json"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestPruneInbox_RemovesOldEntries verifies PruneInbox walks the inbox and
// removes chatID subdirs older than the retention window, while leaving
// recent ones intact.
func TestPruneInbox_RemovesOldEntries(t *testing.T) {
	inbox := t.TempDir()
	old := filepath.Join(inbox, "oc_old", "prompt1")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "f.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate the chat dir's mtime to 30 days ago.
	oldTime := time.Now().AddDate(0, 0, -30)
	if err := os.Chtimes(filepath.Join(inbox, "oc_old"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	recent := filepath.Join(inbox, "oc_recent", "prompt2")
	if err := os.MkdirAll(recent, 0o700); err != nil {
		t.Fatal(err)
	}

	d := NewDispatcher(&fakeSink{}, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetFilePipeline(&fakeDownloader{}, fileconvert.New(fileconvert.Options{}), inbox, 5<<20, mustParseTestTemplate(t))
	d.PruneInbox(7 * 24 * time.Hour)

	if _, err := os.Stat(filepath.Join(inbox, "oc_old")); !os.IsNotExist(err) {
		t.Errorf("old dir should be pruned, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(inbox, "oc_recent")); err != nil {
		t.Errorf("recent dir should remain, got err=%v", err)
	}
}
