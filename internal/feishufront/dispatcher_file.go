package feishufront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/fileconvert"
	"github.com/justphantom/lark-bridge/internal/log"
)

// inboxDirPerm / inboxFilePerm mirror streamarchive's stance: the inbox
// carries user uploads (potentially sensitive) so the directory is 0700 and
// files are 0600. The umask on mkdir/open strengthens these; a shared host
// must still take care that state_dir itself is not world-readable.
const (
	inboxDirPerm  = 0o700
	inboxFilePerm = 0o600
)

// supportedUploadExt is the whitelist the file pipeline accepts. Kept inline
// (not in fileconvert) because the dispatcher also uses it to short-circuit
// before the download: an unsupported extension rejected here saves a
// round-trip to the IM resources API.
var supportedUploadExt = map[string]bool{
	".docx":     true,
	".md":       true,
	".markdown": true,
	".txt":      true,
}

// handleFileMessage materialises one uploaded file into a .md the bound
// backend can Read, then forwards a prompt that names the .md path.
//
// The pipeline is:
//  1. Resolve the bound backend up front so an early "backend offline"
//     notice fires before we touch the network or disk.
//  2. Validate FileKey + extension from the parsed message content.
//  3. Download to {inboxDir}/{chatID}/{promptID}/{original-name} via the
//     FileDownloader (lark REST IM resources endpoint).
//  4. Convert via the fileconvert.Converter (docx → pandoc, md/txt → copy).
//  5. Build the prompt text: filename + absolute .md path + the user's
//     accompanying text (if any) + a directive that points the agent at the
//     file.
//  6. Delegate to dispatchPrompt so the rest of the turn machinery (progress
//     card, turn tracking, SSE Event) is identical to a text turn.
//
// Each error path emits a notice so the user gets feedback rather than
// silence; the inbox dir for this prompt is left in place (the retention
// sweep cleans it later) so a failed conversion's source is still available
// for diagnosis.
func (d *Dispatcher) handleFileMessage(ctx context.Context, msg *feishu.IncomingMessage) error {
	// Early routing check: do this before any download so an offline backend
	// surfaces in <1ms instead of after a multi-second IM resource fetch.
	if d.router == nil {
		return d.notice(ctx, msg.ChatID, "error", "路由未就绪", "前端路由尚未初始化")
	}
	if _, err := d.router.Resolve(msg.ChatID); err != nil {
		return d.notice(ctx, msg.ChatID, "error", "路由失败", err.Error())
	}

	fileName := msg.FileName
	if fileName == "" {
		// Feishu normally sends file_name in Content for file-type messages;
		// fall back to parsing Content ourselves for forward compatibility if
		// a future variant omits the parsed field.
		fileName = parseFileNameFromContent(msg.Content)
	}
	if msg.FileKey == "" || fileName == "" {
		return d.notice(ctx, msg.ChatID, "warning", "文件信息缺失",
			"无法解析文件标识，请重新上传")
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	if !supportedUploadExt[ext] {
		return d.notice(ctx, msg.ChatID, "info", "暂不支持的文件类型",
			"仅支持 docx / md / markdown / txt，当前上传: "+ext)
	}

	// Per-prompt inbox: {inboxDir}/{chatID}/{promptID}/. MkdirAll keeps the
	// chat-scoped subdir layout the retention sweep walks over. The raw
	// upload lands under `raw/` so a `.md` source can never collide with the
	// converted destination (which is `<base>.md` at the prompt root).
	promptID := msg.MessageID
	dir := filepath.Join(d.inboxDir, sanitizePathElement(msg.ChatID), sanitizePathElement(promptID))
	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, inboxDirPerm); err != nil {
		return d.notice(ctx, msg.ChatID, "error", "存储失败",
			"无法创建接收目录: "+err.Error())
	}

	srcPath := filepath.Join(rawDir, sanitizePathElement(fileName))
	// Append a small suffix if the original name collides with a prior upload
	// in the same prompt (rare, but Feishu lets a user reply twice to the
	// same card); the .md destination uses the same base so they pair up.
	if _, err := os.Stat(srcPath); err == nil {
		srcPath = filepath.Join(rawDir, fmt.Sprintf("upload-%d%s", time.Now().UnixNano(), ext))
	}

	// Download with a hard size ceiling so a runaway upload or a misbehaving
	// server cannot exhaust memory. The lark REST layer already caps responses
	// at downloadMaxBytes; we re-check against inboxMaxSize after the copy so
	// the user-facing limit (file_convert.max_file_size) wins over the wire
	// default when tighter.
	body, err := d.fileDownloader.DownloadFile(ctx, msg.MessageID, msg.FileKey, "file")
	if err != nil {
		return d.notice(ctx, msg.ChatID, "error", "下载失败",
			"无法从飞书获取文件: "+err.Error())
	}
	defer func() { _ = body.Close() }()
	n, err := d.saveInboxFile(srcPath, body)
	if err != nil {
		return d.notice(ctx, msg.ChatID, "error", "存储失败", err.Error())
	}
	if d.inboxMaxSize > 0 && n > d.inboxMaxSize {
		_ = os.Remove(srcPath)
		return d.notice(ctx, msg.ChatID, "warning", "文件过大",
			fmt.Sprintf("上传 %s 超过 %s 上限", humanByteSize(n), humanByteSize(d.inboxMaxSize)))
	}

	// Convert. The destination is `<base>.md` at the prompt root: srcPath is
	// under raw/, so even a `.md` source (base == name) cannot overwrite
	// itself. The agent always reads this stable name regardless of the
	// original extension.
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	dstPath := filepath.Join(dir, sanitizePathElement(base+".md"))
	if err := d.fileConverter.Convert(ctx, srcPath, dstPath); err != nil {
		if errors.Is(err, fileconvert.ErrUnsupported) {
			return d.notice(ctx, msg.ChatID, "info", "暂不支持的文件类型",
				"仅支持 docx / md / markdown / txt")
		}
		l := d.logger.Load()
		if l != nil {
			l.Warn("file convert failed",
				log.FieldChatID, msg.ChatID,
				log.FieldMessageID, msg.MessageID,
				log.FieldError, err.Error(),
				"src", filepath.Base(srcPath))
		}
		return d.notice(ctx, msg.ChatID, "error", "转换失败",
			"无法将文件转为 Markdown: "+err.Error())
	}

	// Make the path absolute before giving it to the agent: relative paths
	// resolve against the agent's CWD (the binding's Directory), which has
	// no relation to the inbox location and would confuse the Read tool.
	absDst, err := filepath.Abs(dstPath)
	if err != nil {
		absDst = dstPath
	}
	promptText, err := d.executeFilePromptTemplate(fileName, absDst, msg)
	if err != nil {
		// The template already parsed at config Load; reaching here means a
		// runtime execution failure (e.g. a custom FuncMap entry panicking).
		// Log loudly and surface as a notice so the user knows the upload
		// reached disk but never made it to the agent.
		l := d.logger.Load()
		if l != nil {
			l.Error("file prompt template execute failed",
				log.FieldChatID, msg.ChatID,
				log.FieldMessageID, msg.MessageID,
				log.FieldError, err.Error())
		}
		return d.notice(ctx, msg.ChatID, "error", "渲染失败",
			"提示词模板渲染失败，请联系管理员检查 file_convert.prompt_template："+err.Error())
	}
	if len(promptText) > maxPromptBytes {
		// The prompt body itself stays tiny (a path + a directive), but cap
		// defensively: a hostile FileName or an operator-supplied template
		// repeating over a long UserText could in theory blow past the
		// 64 KiB prompt limit.
		promptText = promptText[:maxPromptBytes]
	}
	return d.dispatchPrompt(ctx, msg, promptText, false)
}

// saveInboxFile copies r into path with a byte counter and the inbox's
// file-mode policy. The copy is bounded by inboxMaxSize+1 so the subsequent
// size check can distinguish "just over" from "runaway" precisely.
func (d *Dispatcher) saveInboxFile(path string, r io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, inboxFilePerm)
	if err != nil {
		return 0, fmt.Errorf("无法创建文件: %w", err)
	}
	// Bound the copy: a LimitReader of inboxMaxSize+1 lets the size check
	// afterwards see exactly-one-byte-over for an oversize upload, instead of
	// pulling a multi-GB body fully into memory.
	limit := d.inboxMaxSize + 1
	if limit <= 0 {
		limit = (30 << 20) + 1 // fileconvert default backstop
	}
	n, err := io.Copy(f, io.LimitReader(r, limit))
	if cErr := f.Close(); err == nil {
		err = cErr
	}
	if err != nil {
		_ = os.Remove(path)
		return n, fmt.Errorf("写入失败: %w", err)
	}
	return n, nil
}

// parseFileNameFromContent extracts file_name from a file-type message's raw
// Content JSON. Used only when the parsed field was empty (a forward-
// compatibility fallback). Returns "" on any parse failure.
func parseFileNameFromContent(content string) string {
	if content == "" {
		return ""
	}
	var c struct {
		FileName string `json:"file_name"`
		FileKey  string `json:"file_key"`
	}
	if err := json.Unmarshal([]byte(content), &c); err != nil {
		return ""
	}
	return c.FileName
}

// filePromptVars is the variable bag handed to file_convert.prompt_template.
// Kept as a struct (not a map) so the field names are the single source of
// truth for template variable spelling — a typo'd {{.Filename}} surfaces at
// render time as a clear "map has no entry for key" error rather than
// silently rendering empty.
type filePromptVars struct {
	FileName string
	Path     string
	UserText string
}

// executeFilePromptTemplate renders the dispatcher's promptTemplate with one
// upload's variables. The template is parsed once at config Load time, so a
// failure here is a runtime execution error (custom FuncMap panic or an IO
// error inside the template's writer), not a syntax error. The caller is
// expected to surface a notice on error; the dispatcher never falls back to
// a compiled-in template because no such template exists by design.
func (d *Dispatcher) executeFilePromptTemplate(fileName, absPath string, msg *feishu.IncomingMessage) (string, error) {
	vars := filePromptVars{
		FileName: fileName,
		Path:     absPath,
		UserText: userTextFromFileMessage(msg),
	}
	var b strings.Builder
	if err := d.promptTemplate.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}

// userTextFromFileMessage returns any user-authored text that accompanied the
// upload. Feishu's file-type message itself carries no body text; the only
// channel is a quote/reply parent (rare for the bridge's group flow). We
// surface it when present so the agent does not lose the user's intent.
func userTextFromFileMessage(msg *feishu.IncomingMessage) string {
	// Today there is no parsed "parent text" field on IncomingMessage; we
	// leave the hook in place so a future enrichment (e.g. parent quote
	// extraction) slots in here without rewriting the prompt builder.
	return strings.TrimSpace(msg.Content)
}

// sanitizePathElement strips path separators and ".." from a caller-supplied
// identifier (chatID, messageID, file name) before splicing it into the
// inbox path. Without this an attacker controlling a chatID (not actually
// possible today, but the IDs are opaque strings) could escape the inbox
// root via "../". We keep it conservative: keep alnum, dash, underscore,
// dot; replace anything else with "_".
func sanitizePathElement(s string) string {
	if s == "" {
		return "unknown"
	}
	out := make([]byte, 0, len(s))
	for i := range s {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z',
			c >= 'A' && c <= 'Z',
			c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	// Collapse ".." sequences that survived via mixed-case or odd placements;
	// the strict allowlist above already forbids them in practice, this is a
	// belt-and-braces guard against future edits widening the charset.
	cleaned := strings.ReplaceAll(string(out), "..", "_")
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}

// humanByteSize renders a byte count as a compact human-readable string for
// use in user-facing notices. Uses 1024-based units (KiB/MiB) to match the
// existing "KiB" usage in maxPromptBytes notices.
func humanByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	suffix := []string{"KiB", "MiB", "GiB", "TiB"}
	if exp >= len(suffix) {
		exp = len(suffix) - 1
	}
	return fmt.Sprintf("%.1f%s", float64(n)/float64(div), suffix[exp])
}

// PruneInbox walks the inbox's chatID subdirs and removes any whose mtime is
// older than retention. Called once at feishu-front startup (after
// SetFilePipeline) so a long-lived deployment does not accumulate stale
// uploads. Best-effort: errors are logged but never fatal — a read-only
// state_dir should not wedge startup.
//
// The inbox directory is created by wireFilePipeline with mode 0700 and is
// owned by the bridge process; symlinks cannot be planted there by an
// external attacker without already having compromised the host, so the
// TOCTOU concern gosec G122 raises for arbitrary Walk+RemoveAll does not
// apply here.
func (d *Dispatcher) PruneInbox(retention time.Duration) {
	if d.inboxDir == "" || retention <= 0 {
		return
	}
	cutoff := time.Now().Add(-retention)
	l := d.logger.Load()
	walkErr := filepath.WalkDir(d.inboxDir, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			// Skip unreadable entries; the inbox is best-effort and a single
			// unreadable subdir should not abort the whole sweep.
			return nil //nolint:nilerr // documented best-effort skip
		}
		if !e.IsDir() {
			return nil
		}
		if path == d.inboxDir {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil //nolint:nilerr // best-effort: unreadable entry, skip
		}
		if info.ModTime().Before(cutoff) {
			if rmErr := os.RemoveAll(path); rmErr != nil && l != nil { //nolint:gosec // G122: inbox is 0700 process-owned, see func doc
				l.Warn("inbox prune: remove failed",
					log.FieldPath, path, log.FieldError, rmErr.Error())
			}
			return filepath.SkipDir
		}
		return nil
	})
	if walkErr != nil && l != nil {
		l.Warn("inbox prune walk failed", log.FieldPath, d.inboxDir, log.FieldError, walkErr.Error())
	}
}
