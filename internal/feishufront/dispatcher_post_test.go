package feishufront

import (
	"context"
	"io"
	"net/http"
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

// testPostPromptTemplate is the canonical post template copied from
// config.example.json. Kept inline so dispatcher tests do not depend on the
// config package's on-disk example.
const testPostPromptTemplate = `用户发送了一条富文本消息并已转为 Markdown。
主体内容已保存到：{{.Path}}
请先用 Read 工具读取该文件，其中可能包含图片引用（vision-capable 的 agent 可直接读取图片路径）。{{if .UserText}}

用户的附加说明：{{.UserText}}{{end}}`

func mustParseTestPostTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.New("test-post").Parse(testPostPromptTemplate)
	if err != nil {
		t.Fatalf("parse test post template: %v", err)
	}
	return tmpl
}

// headeredReader wraps a string body with a fake Content-Type header so the
// dispatcher's downloadImageStream can pick up the image MIME without a real
// HTTP server. Implements io.ReadCloser + the optional Header() method the
// dispatcher's type-assertion looks for.
type headeredReader struct {
	body   string
	header http.Header
	read   int
	closed bool
}

func (h *headeredReader) Read(p []byte) (int, error) {
	if h.closed || h.read >= len(h.body) {
		return 0, io.EOF
	}
	n := copy(p, h.body[h.read:])
	h.read += n
	return n, nil
}

func (h *headeredReader) Close() error        { h.closed = true; return nil }
func (h *headeredReader) Header() http.Header { return h.header }

// headerFakeDownloader serves per-image-key bodies with associated headers,
// recording each call. Used to verify image materialisation paths including
// Content-Type → extension inference.
type headerFakeDownloader struct {
	bodies  map[string]string // image_key → body
	headers map[string]string // image_key → Content-Type
	errs    map[string]error  // image_key → forced error
	calls   []string          // recorded keys in call order
}

func (f *headerFakeDownloader) DownloadFile(_ context.Context, _, key, _ string) (io.ReadCloser, error) {
	f.calls = append(f.calls, key)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	body := f.bodies[key]
	hdr := http.Header{}
	if ct := f.headers[key]; ct != "" {
		hdr.Set("Content-Type", ct)
	}
	return &headeredReader{body: body, header: hdr}, nil
}

// wirePostDispatcher builds a Dispatcher with the post pipeline fully wired
// (file_convert.enabled + post_prompt_template). Returns the dispatcher,
// the bound backend's event channel, the downloader, and the sink for
// notice inspection.
func wirePostDispatcher(t *testing.T, dl *headerFakeDownloader) (*Dispatcher, *BackendConn, *headerFakeDownloader, *fakeSink) {
	t.Helper()
	if dl == nil {
		dl = &headerFakeDownloader{bodies: map[string]string{}, headers: map[string]string{}}
	}
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	const backendID = "claude-1"
	reg.Register(backendID, "claude")
	conn, _ := reg.Get(backendID)
	d := NewDispatcher(sink, reg, NewTurnManager(), boundRouter{backendID: backendID})
	d.SetFilePipeline(dl, fileconvert.New(fileconvert.Options{}), t.TempDir(), 5<<20,
		mustParseTestTemplate(t), mustParseTestPostTemplate(t))
	return d, conn, dl, sink
}

// postMessageWith builds an IncomingMessage for a post-type Feishu message
// from a pre-parsed Post AST. Content stays empty (ParsePost's input is not
// needed at this layer; the dispatcher uses msg.Post directly).
func postMessageWith(post *feishu.Post) *feishu.IncomingMessage {
	return &feishu.IncomingMessage{
		EventID:   "evt_post",
		MessageID: "om_post",
		ChatID:    "oc_chat",
		MsgType:   "post",
		Post:      post,
	}
}

// TestHandlePostIncoming_NilPostNotice verifies a post message whose AST
// failed to parse surfaces as a notice and never reaches dispatchPrompt.
func TestHandlePostIncoming_NilPostNotice(t *testing.T) {
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	conn, _ := reg.Get("claude-1")
	d := NewDispatcher(sink, reg, NewTurnManager(), boundRouter{backendID: "claude-1"})
	// Wire the full pipeline so we know the nil-Post guard fires before any
	// pipeline gating does.
	d.SetFilePipeline(&headerFakeDownloader{bodies: map[string]string{}}, fileconvert.New(fileconvert.Options{}),
		t.TempDir(), 5<<20, mustParseTestTemplate(t), mustParseTestPostTemplate(t))

	err := d.DispatchIncoming(context.Background(), &feishu.IncomingMessage{
		EventID:   "evt_nil",
		MessageID: "om_nil",
		ChatID:    "oc_chat",
		MsgType:   "post",
		Post:      nil,
	})
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 notice, got %d", len(sink.sends))
	}
	if !strings.Contains(string(sink.sends[0].card), "解析失败") {
		t.Errorf("notice missing 解析失败: %s", sink.sends[0].card)
	}
	select {
	case ev := <-conn.eventCh:
		t.Fatalf("no prompt expected, got %+v", ev)
	default:
	}
}

// TestHandlePostMessage_TextOnly verifies a post with no images reaches the
// backend as a Prompt whose body.md path is named and is readable.
func TestHandlePostMessage_TextOnly(t *testing.T) {
	d, conn, _, sink := wirePostDispatcher(t, nil)
	post := &feishu.Post{
		Title: "标题",
		Blocks: [][]feishu.PostNode{
			{{Tag: "text", Text: "看 "}, {Tag: "a", Text: "链接", Href: "https://x"}},
		},
	}
	if err := d.DispatchIncoming(context.Background(), postMessageWith(post)); err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 placeholder send, got %d", len(sink.sends))
	}
	select {
	case ev := <-conn.eventCh:
		if ev.Type != protocol.TypePrompt {
			t.Fatalf("want prompt, got %q", ev.Type)
		}
		text := ev.Prompt.Text
		if !strings.Contains(text, "body.md") {
			t.Errorf("prompt missing body.md path: %q", text)
		}
		// body.md must exist and contain the rendered Markdown.
		idx := strings.Index(text, "：")
		if idx < 0 {
			t.Fatalf("cannot isolate path: %q", text)
		}
		pathLine := text[idx+len("："):]
		end := strings.IndexByte(pathLine, '\n')
		if end < 0 {
			end = len(pathLine)
		}
		abs := strings.TrimSpace(pathLine[:end])
		got, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("read body.md: %v", err)
		}
		if !strings.Contains(string(got), "# 标题") {
			t.Errorf("body.md missing title: %q", got)
		}
		if !strings.Contains(string(got), "看 [链接](https://x)") {
			t.Errorf("body.md missing rendered link: %q", got)
		}
	case <-waitTimeoutChan():
		t.Fatal("no prompt event received")
	}
}

// TestHandlePostMessage_AllImagesOK verifies images materialise to disk and
// the body.md carries Markdown references pointing at the saved paths.
func TestHandlePostMessage_AllImagesOK(t *testing.T) {
	dl := &headerFakeDownloader{
		bodies: map[string]string{
			"img_v3_a": "PNG_BODY_A",
			"img_v3_b": "JPEG_BODY_B",
		},
		headers: map[string]string{
			"img_v3_a": "image/png",
			"img_v3_b": "image/jpeg",
		},
	}
	d, conn, _, sink := wirePostDispatcher(t, dl)
	post := &feishu.Post{
		Blocks: [][]feishu.PostNode{
			{
				{Tag: "text", Text: "两张图："},
				{Tag: "img", ImageKey: "img_v3_a"},
				{Tag: "img", ImageKey: "img_v3_b"},
			},
		},
	}
	if err := d.DispatchIncoming(context.Background(), postMessageWith(post)); err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(dl.calls) != 2 {
		t.Fatalf("downloader called %d times, want 2", len(dl.calls))
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 placeholder send, got %d", len(sink.sends))
	}

	select {
	case ev := <-conn.eventCh:
		if !strings.Contains(ev.Prompt.Text, "body.md") {
			t.Fatalf("prompt missing body.md: %q", ev.Prompt.Text)
		}
		// Locate body.md path and verify both images on disk.
		idx := strings.Index(ev.Prompt.Text, "：")
		pathLine := ev.Prompt.Text[idx+len("："):]
		end := strings.IndexByte(pathLine, '\n')
		if end < 0 {
			end = len(pathLine)
		}
		abs := strings.TrimSpace(pathLine[:end])
		bodyDir := filepath.Dir(abs)
		img1 := filepath.Join(bodyDir, "img-001.png")
		img2 := filepath.Join(bodyDir, "img-002.jpg")
		if got, err := os.ReadFile(img1); err != nil || string(got) != "PNG_BODY_A" {
			t.Errorf("img-001.png = %q, err=%v", got, err)
		}
		if got, err := os.ReadFile(img2); err != nil || string(got) != "JPEG_BODY_B" {
			t.Errorf("img-002.jpg = %q, err=%v", got, err)
		}
		body, _ := os.ReadFile(abs)
		if !strings.Contains(string(body), "![图片]") {
			t.Errorf("body.md missing image ref: %q", body)
		}
	case <-waitTimeoutChan():
		t.Fatal("no prompt event received")
	}
}

// TestHandlePostMessage_PartialImageFailure verifies one failed download
// degrades to a placeholder while the other image still materialises. The
// body.md and prompt must still be sent.
func TestHandlePostMessage_PartialImageFailure(t *testing.T) {
	dl := &headerFakeDownloader{
		bodies: map[string]string{"img_ok": "OK_BODY"},
		errs:   map[string]error{"img_bad": errFake()},
	}
	d, conn, _, sink := wirePostDispatcher(t, dl)
	post := &feishu.Post{
		Blocks: [][]feishu.PostNode{
			{
				{Tag: "img", ImageKey: "img_bad"},
				{Tag: "img", ImageKey: "img_ok"},
			},
		},
	}
	if err := d.DispatchIncoming(context.Background(), postMessageWith(post)); err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 placeholder send, got %d", len(sink.sends))
	}
	select {
	case ev := <-conn.eventCh:
		idx := strings.Index(ev.Prompt.Text, "：")
		pathLine := ev.Prompt.Text[idx+len("："):]
		end := strings.IndexByte(pathLine, '\n')
		if end < 0 {
			end = len(pathLine)
		}
		abs := strings.TrimSpace(pathLine[:end])
		bodyDir := filepath.Dir(abs)
		// img_ok materialised; img_bad did not.
		if _, err := os.Stat(filepath.Join(bodyDir, "img-002.png")); err != nil {
			t.Errorf("ok image not materialised: %v", err)
		}
		body, _ := os.ReadFile(abs)
		if !strings.Contains(string(body), "[图片下载失败]") {
			t.Errorf("body.md missing failure placeholder: %q", body)
		}
		if !strings.Contains(string(body), "![图片]") {
			t.Errorf("body.md missing success ref: %q", body)
		}
	case <-waitTimeoutChan():
		t.Fatal("no prompt event received")
	}
}

// TestHandlePostMessage_AllImagesFail verifies all downloads failing still
// produces a complete body.md (with placeholders) and forwards a prompt.
func TestHandlePostMessage_AllImagesFail(t *testing.T) {
	dl := &headerFakeDownloader{
		errs: map[string]error{
			"img_x": errFake(),
			"img_y": errFake(),
		},
	}
	d, conn, _, sink := wirePostDispatcher(t, dl)
	post := &feishu.Post{
		Blocks: [][]feishu.PostNode{
			{{Tag: "text", Text: "before "}, {Tag: "img", ImageKey: "img_x"}, {Tag: "text", Text: " after"}},
			{{Tag: "img", ImageKey: "img_y"}},
		},
	}
	if err := d.DispatchIncoming(context.Background(), postMessageWith(post)); err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	if len(sink.sends) != 1 {
		t.Fatalf("want 1 placeholder send, got %d", len(sink.sends))
	}
	select {
	case ev := <-conn.eventCh:
		idx := strings.Index(ev.Prompt.Text, "：")
		pathLine := ev.Prompt.Text[idx+len("："):]
		end := strings.IndexByte(pathLine, '\n')
		if end < 0 {
			end = len(pathLine)
		}
		body, _ := os.ReadFile(strings.TrimSpace(pathLine[:end]))
		// Text survives, both placeholders present, no image refs.
		if !strings.Contains(string(body), "before") || !strings.Contains(string(body), "after") {
			t.Errorf("body.md lost surrounding text: %q", body)
		}
		if c := strings.Count(string(body), "[图片下载失败]"); c != 2 {
			t.Errorf("want 2 failure placeholders, got %d in: %q", c, body)
		}
		if strings.Contains(string(body), "![图片]") {
			t.Errorf("body.md should not contain successful image ref: %q", body)
		}
	case <-waitTimeoutChan():
		t.Fatal("no prompt event received")
	}
}

// TestHandlePostMessage_OversizedImage verifies an image exceeding
// inboxMaxSize is rejected and replaced with a size-aware placeholder; the
// image file is removed from disk.
func TestHandlePostMessage_OversizedImage(t *testing.T) {
	// Build a body just over 1 MiB; set inboxMaxSize to 1 MiB.
	huge := strings.Repeat("x", (1<<20)+8)
	dl := &headerFakeDownloader{
		bodies:  map[string]string{"img_big": huge},
		headers: map[string]string{"img_big": "image/png"},
	}
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	conn, _ := reg.Get("claude-1")
	d := NewDispatcher(sink, reg, NewTurnManager(), boundRouter{backendID: "claude-1"})
	d.SetFilePipeline(dl, fileconvert.New(fileconvert.Options{}), t.TempDir(), 1<<20,
		mustParseTestTemplate(t), mustParseTestPostTemplate(t))

	post := &feishu.Post{Blocks: [][]feishu.PostNode{{{Tag: "img", ImageKey: "img_big"}}}}
	if err := d.DispatchIncoming(context.Background(), postMessageWith(post)); err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	select {
	case ev := <-conn.eventCh:
		idx := strings.Index(ev.Prompt.Text, "：")
		pathLine := ev.Prompt.Text[idx+len("："):]
		end := strings.IndexByte(pathLine, '\n')
		if end < 0 {
			end = len(pathLine)
		}
		bodyDir := filepath.Dir(strings.TrimSpace(pathLine[:end]))
		// File removed (oversize → saveInboxFile then Remove).
		if _, err := os.Stat(filepath.Join(bodyDir, "img-001.png")); !os.IsNotExist(err) {
			t.Errorf("oversize image file should be removed, got err=%v", err)
		}
		body, _ := os.ReadFile(filepath.Join(bodyDir, "body.md"))
		if !strings.Contains(string(body), "[图片过大：") {
			t.Errorf("body.md missing size placeholder: %q", body)
		}
	case <-waitTimeoutChan():
		t.Fatal("no prompt event received")
	}
}

// TestInferImageExt covers each supported MIME → extension mapping plus the
// conservative default for unknown / missing types.
func TestInferImageExt(t *testing.T) {
	cases := []struct {
		ct   string
		want string
	}{
		{"image/png", ".png"},
		{"image/jpeg", ".jpg"},
		{"image/jpg", ".jpg"},
		{"image/gif", ".gif"},
		{"image/webp", ".webp"},
		{"", ".png"},
		{"image/unknown", ".png"},
		{"not-a-mime", ".png"},
	}
	for _, c := range cases {
		if got := inferImageExt(c.ct); got != c.want {
			t.Errorf("inferImageExt(%q) = %q, want %q", c.ct, got, c.want)
		}
	}
}

// TestStripBotMentionsFromPost verifies @bot and @all are removed from the
// AST in place while regular @user mentions survive.
func TestStripBotMentionsFromPost(t *testing.T) {
	p := &feishu.Post{Blocks: [][]feishu.PostNode{
		{
			{Tag: "text", Text: "hi "},
			{Tag: "at", UserID: "ou_bot", UserName: "Bot"},
			{Tag: "text", Text: " "},
			{Tag: "at", UserID: "ou_user", UserName: "张三"},
		},
		{{Tag: "at", UserID: "all"}},
	}}
	mentions := []feishu.Mention{
		{OpenID: "ou_bot", IsBot: true},
		{OpenID: "all"},
	}
	feishu.StripBotMentionsFromPost(p, mentions)
	if len(p.Blocks[0]) != 3 {
		t.Errorf("block 0: want 3 nodes (bot removed), got %d: %+v", len(p.Blocks[0]), p.Blocks[0])
	}
	if len(p.Blocks[1]) != 0 {
		t.Errorf("block 1: want empty (@all removed), got %+v", p.Blocks[1])
	}
	// Verify the surviving @user node is intact.
	if p.Blocks[0][2].Tag != "at" || p.Blocks[0][2].UserID != "ou_user" {
		t.Errorf("regular @user altered: %+v", p.Blocks[0][2])
	}
}

// TestExecutePostPromptTemplate verifies the post template's three rendering
// paths: with user text, without user text, and the path variable.
func TestExecutePostPromptTemplate(t *testing.T) {
	d, _, _, _ := wirePostDispatcher(t, nil)

	// With user text: both Path and UserText sections render.
	got, err := d.executePostPromptTemplate("/abs/body.md",
		&feishu.IncomingMessage{Content: "请总结"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(got, "/abs/body.md") {
		t.Errorf("missing path: %q", got)
	}
	if !strings.Contains(got, "用户的附加说明：请总结") {
		t.Errorf("missing user text section: %q", got)
	}

	// Without user text: user-text section omitted entirely.
	got, err = d.executePostPromptTemplate("/abs/body.md",
		&feishu.IncomingMessage{Content: ""})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(got, "用户的附加说明") {
		t.Errorf("user text section should be omitted, got: %q", got)
	}
}

// TestPostPipelineDegradedPath verifies the "no post_prompt_template" path:
// post messages still reach the backend as a plain Markdown prompt, with
// images replaced by [图片] placeholders and no inbox written.
func TestPostPipelineDegradedPath(t *testing.T) {
	sink := &fakeSink{}
	reg := NewBackendRegistry()
	reg.Register("claude-1", "claude")
	conn, _ := reg.Get("claude-1")
	d := NewDispatcher(sink, reg, NewTurnManager(), boundRouter{backendID: "claude-1"})
	// File pipeline enabled (prompt_template set) but post template nil.
	d.SetFilePipeline(&headerFakeDownloader{bodies: map[string]string{}},
		fileconvert.New(fileconvert.Options{}), t.TempDir(), 5<<20,
		mustParseTestTemplate(t), nil)

	post := &feishu.Post{Blocks: [][]feishu.PostNode{
		{{Tag: "text", Text: "before "}, {Tag: "img", ImageKey: "img_x"}, {Tag: "text", Text: " after"}},
	}}
	err := d.DispatchIncoming(context.Background(), postMessageWith(post))
	if err != nil {
		t.Fatalf("DispatchIncoming: %v", err)
	}
	select {
	case ev := <-conn.eventCh:
		// Degraded path: prompt text IS the rendered body (not a path ref).
		text := ev.Prompt.Text
		if !strings.Contains(text, "before") || !strings.Contains(text, "after") {
			t.Errorf("degraded prompt lost surrounding text: %q", text)
		}
		if !strings.Contains(text, "[图片]") {
			t.Errorf("degraded prompt missing [图片] placeholder: %q", text)
		}
		if strings.Contains(text, "body.md") {
			t.Errorf("degraded prompt should not name body.md: %q", text)
		}
	case <-waitTimeoutChan():
		t.Fatal("no prompt event received")
	}
}

// waitTimeoutChan returns a 2s timer channel for "no event expected"
// guards. Declared separately from dispatcher_file_test.go's waitTimeout
// because that helper's signature is local to its file's pre-existing
// imports; keeping the post tests self-contained avoids touching it.
func waitTimeoutChan() <-chan time.Time {
	return time.After(2 * time.Second)
}
