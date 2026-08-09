package bridgebase

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// MaxSendFileSize is the per-file ceiling for /send, matching the Feishu IM
// file-message API limit. Enforced before base64 (which would inflate 33%)
// so a too-large file is rejected with a friendly notice rather than a
// mid-upload failure or an oversized IPC payload.
const MaxSendFileSize = 30 << 20 // 30 MiB

// sendDirOptionLimit caps one picker card's option count at the button-render
// ceiling (cardkit.MaxCardElements − header/markdown/footer = 45), NOT the
// global maxQuestionOptions(100): the directory browser renders as
// immediate-click buttons (no select_static/form_submit) so a pick commits in
// one click without opening Feishu's long form/select callback window — the
// window that silently reverted the picker PATCH (回弹) and forced the "已发送"
// outcome onto a duplicate card. A huge directory truncates; the user reaches
// cut entries via /send <path>.
const sendDirOptionLimit = 45

// SafeJoin constrains rel under root, resolving symlinks so a link that points
// outside root cannot escape (send-file-design.md §5.1). root and the joined
// target are both Abs/Clean-ed first; EvalSymlinks then resolves the real
// path, and a Rel check confirms it did not land above root. Returns the
// resolved absolute path or an error a backend turns into a user notice.
// Errors deliberately carry no absolute server paths — the message lands in
// the chat verbatim, and leaking /home/... layouts aids an attacker.
func SafeJoin(root, rel string) (string, error) {
	absRoot, err := ResolveRoot(root)
	if err != nil {
		return "", fmt.Errorf("解析根目录失败")
	}
	rel = filepath.Clean(rel)
	target := filepath.Join(absRoot, rel)
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("路径不存在或无法访问：%s", rel)
	}
	if !withinRoot(absRoot, resolved) {
		return "", fmt.Errorf("路径越界：%s", rel)
	}
	return resolved, nil
}

// ResolveRoot returns root's absolute, symlink-resolved form. The resolution
// matters when the bound directory is itself a symlink (~/proj → /data/proj):
// EvalSymlinks on the target resolves every component including root's, so
// comparing against the unresolved root would false-positive every file as
// "越界". A root that cannot be resolved falls back to the Abs/Clean form.
func ResolveRoot(root string) (string, error) {
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	if r, rerr := filepath.EvalSymlinks(absRoot); rerr == nil {
		absRoot = r
	}
	return absRoot, nil
}

// withinRoot reports whether resolved lies at or under absRoot. The check is
// rel == ".." or a "../" prefix — a plain ".." prefix would also reject a
// legitimate sibling named "..foo".
func withinRoot(absRoot, resolved string) bool {
	rel, err := filepath.Rel(absRoot, resolved)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// BuildSendOptions renders one directory's entries as the picker option list:
// "⬆️ .." when not at root, then sorted subdirectories ("📁 name/"), then
// sorted files ("📄 name"). Dotfiles are hidden. The list is capped at
// sendDirOptionLimit (dirs before files) so a vast directory never produces a
// card Feishu rejects; a user reaches truncated entries via /send <path>.
func BuildSendOptions(currDir, rootDir string, entries []os.DirEntry) []string {
	absCurr, _ := filepath.Abs(filepath.Clean(currDir))
	absRoot, _ := filepath.Abs(filepath.Clean(rootDir))
	var dirs, files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, "📁 "+name+"/")
		} else {
			files = append(files, "📄 "+name)
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)
	options := make([]string, 0, 1+len(dirs)+len(files))
	if absCurr != absRoot {
		options = append(options, "⬆️ ..")
	}
	options = append(options, dirs...)
	options = append(options, files...)
	if len(options) > sendDirOptionLimit {
		options = options[:sendDirOptionLimit]
	}
	return options
}

// ParseSendOption decodes one BuildSendOptions entry back into a kind and a
// raw name: "up" (..), "dir" (name without trailing slash), or "file". An
// unrecognised string yields ("", "") so the caller can bail out safely.
func ParseSendOption(choice string) (kind, name string) {
	switch {
	case choice == "⬆️ ..":
		return "up", ""
	case strings.HasPrefix(choice, "📁 "):
		return "dir", strings.TrimSuffix(strings.TrimPrefix(choice, "📁 "), "/")
	case strings.HasPrefix(choice, "📄 "):
		return "file", strings.TrimPrefix(choice, "📄 ")
	}
	return "", ""
}

// ReadFilePayload reads one file, enforces the size cap, and wraps it as a
// TypeFile payload (base64 content). Shared by the Core-based bridges and
// miniagent so the size/encoding/escape policy lives in one place. updateMessageID
// is threaded through so the frontend can PATCH the picker card the user just
// clicked instead of sending a standalone result notice.
//
// root is the chat's bound directory: the path is re-validated here (symlink
// resolution + containment) at read time rather than only at selection time,
// closing the swap-a-symlink TOCTOU window between the two. Non-regular files
// (FIFO/device) are rejected — os.Stat reports size 0 for a FIFO, which would
// otherwise pass the cap and then block the read forever. The read itself is
// capped at MaxSendFileSize+1 so a file growing after Stat cannot slip past.
// Errors carry no absolute server paths (they surface in the chat verbatim).
func ReadFilePayload(chatID, fileName, root, path, updateMessageID string) (*protocol.FilePayload, error) {
	absRoot, err := ResolveRoot(root)
	if err != nil {
		return nil, fmt.Errorf("工作目录无效")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("路径不存在或无法访问：%s", fileName)
	}
	if !withinRoot(absRoot, resolved) {
		return nil, fmt.Errorf("路径越界：%s", fileName)
	}
	// Stat before Open: opening a FIFO for read blocks until a writer shows
	// up, so the regular-file gate must happen on Stat (which never blocks).
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("文件不存在或无权访问：%s", fileName)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s 不是常规文件，无法发送", fileName)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("文件 %s 为空，不支持发送空文件", fileName)
	}
	if info.Size() > MaxSendFileSize {
		return nil, fmt.Errorf("文件 %s 为 %s，超过 %s 上限", fileName, sendFileSize(info.Size()), sendFileSize(MaxSendFileSize))
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("文件不存在或无权访问：%s", fileName)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxSendFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("读取文件失败：%s", fileName)
	}
	if int64(len(data)) > MaxSendFileSize {
		return nil, fmt.Errorf("文件 %s 超过 %s 上限", fileName, sendFileSize(MaxSendFileSize))
	}
	return &protocol.FilePayload{
		ChatID:          chatID,
		FileName:        fileName,
		Content:         base64.StdEncoding.EncodeToString(data),
		UpdateMessageID: updateMessageID,
	}, nil
}

// sendFileSize renders a byte count compactly for the over-limit notice.
func sendFileSize(n int64) string {
	const mi = int64(1) << 20
	if n < mi {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/float64(mi))
}

// CmdSend is the Core-based /send entry point shared by claude/opencode backs.
// rootDir is the chat's bound working directory (the wrapper resolves it from
// the router binding). With args it sends the relative path directly; without
// args it spawns the directory browser. Either path runs on the Core's
// process-lifetime context (file read + base64 + emit can outlast the
// dispatcher's 15s command timeout), so the handler returns Handled=true
// immediately and emits its own TypeFile / notice.
func CmdSend(ctx context.Context, c *Core, chatID, rootDir string, args []string) (cmdutil.Result, error) {
	replyToID := ReplyToID(ctx)
	if rootDir == "" {
		return cmdutil.ErrorResult("尚未设置工作目录。发送 `/cd` 选择一个项目目录后再发送文件。")
	}
	absRoot, err := ResolveRoot(rootDir)
	if err != nil {
		return cmdutil.ErrorResult("工作目录无效：%v", err)
	}
	if len(args) > 0 {
		// Join rather than args[0]: Fields-splitting breaks paths containing
		// spaces, and silently truncating to the first word would send the
		// wrong file or report a confusing "not exists".
		target, jerr := SafeJoin(absRoot, strings.Join(args, " "))
		if jerr != nil {
			return cmdutil.ErrorResult("%v", jerr)
		}
		GoSafe(c.Logger, "send:direct", func() { emitSendFile(c, chatID, absRoot, target, "") })
		return cmdutil.Result{Handled: true}, nil
	}
	GoSafe(c.Logger, "send:browser", func() { runSendBrowser(c, chatID, replyToID, absRoot) })
	return cmdutil.Result{Handled: true}, nil
}

// runSendBrowser is the multi-round directory picker: each round reads the
// current dir, builds the option list, asks one question via Core.AskAndWait,
// and descends / ascends / sends on the user's pick. AskAndWait's appCtx +
// 9-minute wait outlast the dispatcher timeout, and its TakeOverProgress
// morphs the /send progress card into the picker card each round.
//
// Round 1 morphs the progress card; every later round PATCHes that SAME card
// in place via AskCardUpdate (QuestionPayload.UpdateMessageID), so descending
// into a directory updates the picker rather than leaving the prior card
// behind and piling up a new one per level. pickerMsgID carries the round-1
// card's message_id across iterations; AskCardUpdate returns the message_id
// the click actually landed on (the same card unless the frontend fell back to
// a standalone send).
func runSendBrowser(c *Core, chatID, replyToID, absRoot string) {
	currDir := absRoot
	pickerMsgID := ""
	for {
		entries, err := os.ReadDir(currDir)
		if err != nil {
			c.EmitNoticeLogged(chatID, "error", "发送失败", "读取目录失败："+err.Error())
			return
		}
		options := BuildSendOptions(currDir, absRoot, entries)
		if len(options) == 0 {
			c.EmitNoticeLogged(chatID, "warning", "发送文件", "当前目录为空。")
			return
		}
		label := "选择要发送的文件（📁 进入子目录，⬆️ 返回上级）"
		var (
			choice    string
			messageID string
			aerr      error
		)
		if pickerMsgID == "" {
			choice, messageID, aerr = c.AskAndWait(chatID, replyToID, "文件", label, StaticOptions(options), false)
		} else {
			choice, messageID, aerr = c.AskCardUpdate(chatID, pickerMsgID, "文件", label, StaticOptions(options), false)
		}
		if aerr != nil {
			c.EmitNoticeLogged(chatID, "warning", "发送取消", aerr.Error())
			return
		}
		if pickerMsgID == "" {
			pickerMsgID = messageID
		}
		kind, name := ParseSendOption(choice)
		switch kind {
		case "up":
			currDir = sendParentDir(currDir, absRoot)
		case "dir":
			// SafeJoin per transition (not just at the final file pick) so a
			// symlinked directory — or one swapped in mid-browse — cannot walk
			// the browser outside absRoot.
			target, jerr := SafeJoin(currDir, name)
			if jerr != nil {
				c.EmitNoticeLogged(chatID, "error", "发送失败", jerr.Error())
				return
			}
			currDir = target
		case "file":
			target, jerr := SafeJoin(currDir, name)
			if jerr != nil {
				c.EmitNoticeLogged(chatID, "error", "发送失败", jerr.Error())
				return
			}
			// Lock the picker card into a "selected" state BEFORE the upload:
			// ends the interactive binding immediately (no delayed submit-
			// fallback PATCH can race the outcome card) and gives the user a
			// stable card while the file uploads.
			c.AskSelectedCard(chatID, messageID, "已选择 "+name)
			emitSendFile(c, chatID, absRoot, target, messageID)
			return
		default:
			c.EmitNoticeLogged(chatID, "warning", "发送取消", "未识别的选择。")
			return
		}
	}
}

// AskSelectedCard PATCHes a multi-round picker card into its final locked
// "user picked X" state — same single-option question-card trick
// AskCardUpdate uses for intermediate refresh rounds, except the single
// option is the chosen file and the requestID is fresh. Because the frontend
// ends any prior interactive binding on this card when it registers the
// refresh (sendInteractive evicts same-card bindings), the delayed submit
// fallback from the click that JUST selected this file finds no binding and
// never fires; the card stays on the locked bytes until the file-outcome
// frame (green "已发送") replaces them. Best-effort: a failure leaves the
// prior round's card, which the outcome still patches.
func (c *Core) AskSelectedCard(chatID, updateMessageID, label string) {
	if updateMessageID == "" {
		return
	}
	requestID, err := newRequestID()
	if err != nil {
		return
	}
	// A "selected" terminal card stays in place until the file outcome
	// (green "已发送") replaces it. Fire-and-forget like AskCardUpdate.
	c.EmitAsync("", &protocol.Control{
		Type:   protocol.TypeQuestion,
		ChatID: chatID,
		Question: &protocol.QuestionPayload{
			RequestID:       requestID,
			Questions:       []protocol.QuestionItem{{Label: label, Options: []string{label}}},
			UpdateMessageID: updateMessageID,
		},
	})
}

// sendParentDir moves up one level without escaping absRoot. Cleaned Rel
// check guards against a currDir already at root (returns root unchanged).
func sendParentDir(currDir, absRoot string) string {
	parent := filepath.Dir(currDir)
	if !withinRoot(absRoot, parent) {
		return absRoot
	}
	return parent
}

// emitSendFile reads one file into a FilePayload and ships a TypeFile control
// to the frontend, which uploads and sends it. updateMessageID (the picker
// card when set) lets the frontend PATCH that card with the outcome; "" falls
// back to a standalone notice on failure (success is silent — the file itself
// landing in the chat is the confirmation). absRoot is re-enforced inside
// ReadFilePayload at read time.
func emitSendFile(c *Core, chatID, absRoot, path, updateMessageID string) {
	fileName := filepath.Base(path)
	payload, err := ReadFilePayload(chatID, fileName, absRoot, path, updateMessageID)
	if err != nil {
		c.EmitNoticeLogged(chatID, "error", "发送失败", err.Error())
		return
	}
	if err := c.Emit(c.AppCtx, "", &protocol.Control{
		Type:   protocol.TypeFile,
		ChatID: chatID,
		File:   payload,
	}); err != nil {
		c.EmitNoticeLogged(chatID, "error", "发送失败", "递交文件失败："+err.Error())
	}
}
