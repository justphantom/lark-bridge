package feishufront

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/lark-bridge/internal/atomicwrite"
	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/log"
)

// handlePostMessage converts a Feishu post-type message into a Markdown
// body.md on disk, downloading inline images into the prompt inbox, then
// forwards a prompt naming body.md's absolute path to the bound backend.
//
// Behaviour matrix (matches docs/post-rich-text-design.md §7):
//   - Post nil (parse failed at feishu layer) → caller (handlePostIncoming)
//     surfaces a notice; this func is only called when Post != nil.
//   - 0 images → body.md written, prompt sent.
//   - N images, all OK → body.md with ![](path) references, prompt sent.
//   - N images, partial failure → successful images referenced, failed
//     ones replaced with placeholders ([图片下载失败] / [图片过大]);
//     body.md still written, prompt still sent.
//   - All images fail → body.md with all-placeholder image positions;
//     body.md still written, prompt still sent.
//   - body.md write fails (e.g. disk full) → "存储失败" notice; no prompt.
//
// Single-image failure never aborts the post: the agent always receives
// the textual body and a clear signal per image of whether it is readable.
func (d *Dispatcher) handlePostMessage(ctx context.Context, msg *feishu.IncomingMessage) error {
	// Early routing check before any IO so an offline backend surfaces fast.
	if d.router == nil {
		return d.notice(ctx, msg.ChatID, "error", "路由未就绪", "前端路由尚未初始化")
	}
	if _, err := d.router.Resolve(msg.ChatID); err != nil {
		return d.notice(ctx, msg.ChatID, "error", "路由失败", err.Error())
	}

	// Defensive copy of the AST: StripBotMentionsFromPost mutates in place,
	// and we never want to alter the parsed event (kept verbatim for the
	// raw/post.json archive below).
	post := clonePost(msg.Post)
	feishu.StripBotMentionsFromPost(post, msg.Mentions)

	// Per-prompt inbox: {inbox}/{chatID}/{promptID}/. Mirrors handleFileMessage
	// so a single retention sweep covers both file uploads and post images.
	dir := filepath.Join(d.inboxDir,
		sanitizePathElement(msg.ChatID),
		sanitizePathElement(msg.MessageID))
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, inboxDirPerm); err != nil {
		return d.notice(ctx, msg.ChatID, "error", "存储失败",
			"无法创建接收目录: "+err.Error())
	}

	// Archive the raw Content JSON so the post can be re-processed or audited
	// without reaching for the Feishu API. Best-effort: a write failure does
	// not block the conversion (the operator can still see the rendered body).
	if msg.Content != "" {
		_ = atomicwrite.Write(filepath.Join(rawDir, "post.json"), []byte(msg.Content), inboxFilePerm)
	}

	body := d.renderPostBody(ctx, post, msg, dir)
	if strings.TrimSpace(body) == "" {
		// An entirely empty body (e.g. a post that contained only @bot and no
		// other content) has nothing to forward. Drop silently rather than
		// shipping an empty prompt.
		return nil
	}

	// Write body.md via atomicwrite so a crash never leaves a truncated file
	// the agent might race-read. The body lives at the prompt inbox root so
	// relative image references `![](img-001.png)` resolve from the same dir.
	bodyPath := filepath.Join(dir, "body.md")
	if err := atomicwrite.Write(bodyPath, []byte(body), inboxFilePerm); err != nil {
		l := d.logger.Load()
		if l != nil {
			l.Warn("post body.md write failed",
				log.FieldChatID, msg.ChatID,
				log.FieldMessageID, msg.MessageID,
				log.FieldError, err.Error())
		}
		return d.notice(ctx, msg.ChatID, "error", "存储失败",
			"无法保存富文本内容: "+err.Error())
	}

	absBody, err := filepath.Abs(bodyPath)
	if err != nil {
		absBody = bodyPath
	}
	promptText, err := d.executePostPromptTemplate(absBody, msg)
	if err != nil {
		l := d.logger.Load()
		if l != nil {
			l.Error("post prompt template execute failed",
				log.FieldChatID, msg.ChatID,
				log.FieldMessageID, msg.MessageID,
				log.FieldError, err.Error())
		}
		return d.notice(ctx, msg.ChatID, "error", "渲染失败",
			"提示词模板渲染失败，请联系管理员检查 file_convert.post_prompt_template："+err.Error())
	}
	if len(promptText) > maxPromptBytes {
		promptText = promptText[:maxPromptBytes]
	}
	return d.dispatchPrompt(ctx, msg, promptText, false)
}

// renderPostBody walks the AST and produces the Markdown body, downloading
// inline images into dir as it goes. Image download failures degrade to
// inline placeholders; the body always renders so a textual prompt reaches
// the agent regardless of how many images failed.
//
// Image numbering is per-prompt (1-indexed, zero-padded to 3 digits) so the
// agent can correlate references in body.md with files on disk by name.
func (d *Dispatcher) renderPostBody(ctx context.Context, p *feishu.Post, msg *feishu.IncomingMessage, dir string) string {
	var b strings.Builder
	if p.Title != "" {
		b.WriteString("# ")
		b.WriteString(p.Title)
		b.WriteString("\n\n")
	}
	imgIdx := 0
	for i, block := range p.Blocks {
		if i > 0 {
			b.WriteString("\n\n")
		}
		for _, node := range block {
			switch node.Tag {
			case "img":
				imgIdx++
				// materialiseImage always returns a non-empty string:
				// the Markdown reference on success, or a placeholder
				// ([图片下载失败] etc.) on failure. Splice verbatim.
				b.WriteString(d.materializeImage(ctx, msg, dir, node.ImageKey, imgIdx))
			case "media":
				// Videos are not downloadable in this pipeline (agent
				// cannot read them); emit a fixed placeholder so the
				// agent knows a video was attached without losing the
				// structural slot.
				b.WriteString("[视频]")
			default:
				b.WriteString(feishu.RenderNodeToMarkdown(node))
			}
		}
	}
	return b.String()
}

// materializeImage downloads one inline image into dir, returning the
// Markdown reference (success) or a placeholder string (failure). The
// return is always non-empty so the caller can splice it verbatim into the
// rendered body without per-failure branching.
//
// Filename convention: img-NNN.<ext>, where NNN is the per-prompt 1-indexed
// position (zero-padded to 3 digits) and ext is inferred from the response
// Content-Type (defaulting to .png when Feishu omits the header or sends an
// unknown type — PNG is the most universally decodable format).
func (d *Dispatcher) materializeImage(ctx context.Context, msg *feishu.IncomingMessage, dir, imageKey string, idx int) string {
	if imageKey == "" {
		return "[图片：缺少标识]"
	}
	body, header, err := d.downloadImageStream(ctx, msg.MessageID, imageKey)
	if err != nil {
		l := d.logger.Load()
		if l != nil {
			l.Warn("post image download failed",
				log.FieldChatID, msg.ChatID,
				log.FieldMessageID, msg.MessageID,
				"image_key", imageKey,
				log.FieldError, err.Error())
		}
		return "[图片下载失败]"
	}
	defer func() { _ = body.Close() }()
	ext := inferImageExt(header.Get("Content-Type"))
	filename := fmt.Sprintf("img-%03d%s", idx, ext)
	dst := filepath.Join(dir, filename)
	n, err := d.saveInboxFile(dst, body)
	if err != nil {
		l := d.logger.Load()
		if l != nil {
			l.Warn("post image save failed",
				log.FieldChatID, msg.ChatID,
				log.FieldMessageID, msg.MessageID,
				"image_key", imageKey,
				log.FieldError, err.Error())
		}
		return "[图片存储失败]"
	}
	if d.inboxMaxSize > 0 && n > d.inboxMaxSize {
		_ = os.Remove(dst)
		return fmt.Sprintf("[图片过大：%s]", humanByteSize(n))
	}
	abs, _ := filepath.Abs(dst)
	return fmt.Sprintf("![图片](%s)", abs)
}

// downloadImageStream wraps the FileDownloader so the dispatcher can read
// the response Content-Type alongside the body. The underlying
// *lark.limitedReadCloser exposes a Header() method (added when the file
// pipeline was introduced); a downloader that does not expose Header()
// (e.g. a test fake) yields an empty header set, and inferImageExt then
// falls back to .png.
func (d *Dispatcher) downloadImageStream(ctx context.Context, messageID, imageKey string) (io.ReadCloser, http.Header, error) {
	rc, err := d.fileDownloader.DownloadFile(ctx, messageID, imageKey, "image")
	if err != nil {
		return nil, nil, err
	}
	if h, ok := rc.(interface{ Header() http.Header }); ok {
		return rc, h.Header(), nil
	}
	return rc, http.Header{}, nil
}

// inferImageExt maps a Content-Type value to a file extension, defaulting
// to .png when the header is absent or carries an unrecognised type. PNG is
// the most universally decodable format across image readers (claude CLI's
// vision, macOS Preview, browser preview), making it a safe conservative
// default when Feishu omits Content-Type or sends an unusual type.
func inferImageExt(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "", "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ".png"
	}
}

// clonePost returns a deep copy of p so downstream mutations (e.g.
// StripBotMentionsFromPost) do not alter the caller's AST. Required because
// PostNode is a value type held in a slice-of-slices: a shallow clone would
// share the inner slices.
func clonePost(p *feishu.Post) *feishu.Post {
	if p == nil {
		return nil
	}
	out := &feishu.Post{Title: p.Title, Blocks: make([][]feishu.PostNode, len(p.Blocks))}
	for i, block := range p.Blocks {
		row := make([]feishu.PostNode, len(block))
		copy(row, block)
		out.Blocks[i] = row
	}
	return out
}

// postPromptVars is the variable bag handed to file_convert.post_prompt_template.
// Mirrors filePromptVars' design: a struct (not a map) so a typo'd variable
// name surfaces at render time rather than rendering silently empty.
type postPromptVars struct {
	Path     string
	UserText string
}

// executePostPromptTemplate renders the dispatcher's postPromptTemplate
// with one post message's variables. The template is parsed once at config
// Load time; a failure here is a runtime execution error, surfaced as a
// notice so the operator knows the upload reached disk but never reached
// the agent.
func (d *Dispatcher) executePostPromptTemplate(absPath string, msg *feishu.IncomingMessage) (string, error) {
	if d.postPromptTemplate == nil {
		// Pipeline was wired without a post template (file pipeline enabled
		// but post_prompt_template not configured). Fall back to the file
		// template so the prompt is still well-formed rather than empty;
		// the config layer should have caught this at Load time, but the
		// guard keeps the dispatcher from emitting a zero-byte prompt if a
		// future code path relaxes the validation.
		vars := filePromptVars{
			FileName: "body.md",
			Path:     absPath,
			UserText: strings.TrimSpace(postUserText(msg)),
		}
		var b strings.Builder
		if err := d.promptTemplate.Execute(&b, vars); err != nil {
			return "", err
		}
		return b.String(), nil
	}
	vars := postPromptVars{
		Path:     absPath,
		UserText: strings.TrimSpace(postUserText(msg)),
	}
	var b strings.Builder
	if err := d.postPromptTemplate.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}

// postUserText returns user-authored text accompanying a post. Feishu's post
// message itself IS the content; there is no separate body channel. The
// hook stays for symmetry with file uploads and to slot a future
// quote/reply parent extraction without rewriting the template path.
func postUserText(msg *feishu.IncomingMessage) string {
	return msg.Content
}

// renderPostBodyAsTextOnly is the fallback path for when the file pipeline
// is disabled: the post is rendered to a Markdown string and forwarded as
// a plain text prompt, with images replaced by [图片] placeholders (no
// download). This matches "方案 B" semantics from the design doc; it lets
// post messages degrade gracefully even when an operator has not enabled
// inbox storage.
func (d *Dispatcher) renderPostBodyAsTextOnly(msg *feishu.IncomingMessage) string {
	post := clonePost(msg.Post)
	feishu.StripBotMentionsFromPost(post, msg.Mentions)
	return feishu.RenderPostToMarkdown(post)
}
