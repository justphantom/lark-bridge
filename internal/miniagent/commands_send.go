package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdSend delivers a file from the chat's active working directory into the
// chat via the frontend (send-file-design.md). No-arg opens the directory
// browser; a relative path sends directly. miniagent has no bridgebase.Core
// and its own picker/emit, so this reuses only the shared pure helpers
// (SafeJoin / BuildSendOptions / ParseSendOption / ReadFilePayload) and drives
// the askAndWait loop + sendCtrl emit off the miniagent Handler directly.
//
// Like /cd and /model, the browser runs in a goroutine on context.Background
// (the user's click may come minutes later, outliving the turn ctx) and
// returns the "async" sentinel so handleSessionCommand does not emit a notice.
func (h *Handler) cmdSend(_ context.Context, chatID, arg string) (level, title, body string) {
	dir := h.activeDir(chatID)
	if dir == "" {
		return "warning", "未设置目录", "尚未配置工作目录（WORKSPACE_ROOT 为空或未 /cd），无法发送文件。"
	}
	absRoot, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return "error", "发送失败", "工作目录无效：" + err.Error()
	}
	promptID := h.PromptIDForPickers(chatID)
	if arg != "" {
		target, jerr := bridgebase.SafeJoin(absRoot, arg)
		if jerr != nil {
			return "error", "发送失败", jerr.Error()
		}
		go func() { //nolint:gosec // G118: read+emit outlives the turn ctx for large files
			h.emitSendFile(chatID, absRoot, target, "")
		}()
		return "async", "", ""
	}
	go func() { //nolint:gosec // G118: picker outlives the turn ctx — click may come minutes later
		h.runSendBrowser(chatID, promptID, absRoot)
	}()
	return "async", "", ""
}

// runSendBrowser is miniagent's multi-round directory picker for /send. Each
// round reads the current dir, builds the option list via the shared helper,
// asks one question through miniagent.askAndWait, and descends/ascends/sends
// on the user's pick. The chosen card's messageID threads through to
// emitSendFile so the frontend PATCHes that card with the outcome.
func (h *Handler) runSendBrowser(chatID, promptID, absRoot string) {
	currDir := absRoot
	for {
		entries, err := os.ReadDir(currDir)
		if err != nil {
			h.notifyWithPromptID(chatID, promptID, "error", "发送失败", "读取目录失败："+err.Error())
			return
		}
		options := bridgebase.BuildSendOptions(currDir, absRoot, entries)
		if len(options) == 0 {
			h.notifyWithPromptID(chatID, promptID, "warning", "发送文件", "当前目录为空。")
			return
		}
		choice, messageID, err := h.askAndWait(context.Background(), chatID, promptID,
			"选择要发送的文件（📁 进入子目录，⬆️ 返回上级）", options)
		if err != nil {
			h.notifyWithPromptID(chatID, promptID, "warning", "发送取消", err.Error())
			return
		}
		kind, name := bridgebase.ParseSendOption(choice)
		switch kind {
		case "up":
			currDir = sendParentDirMini(currDir, absRoot)
		case "dir":
			// SafeJoin per transition so a symlinked (or mid-browse swapped)
			// directory cannot walk the browser outside absRoot.
			target, jerr := bridgebase.SafeJoin(currDir, name)
			if jerr != nil {
				h.notifyWithPromptID(chatID, promptID, "error", "发送失败", jerr.Error())
				return
			}
			currDir = target
		case "file":
			target, jerr := bridgebase.SafeJoin(currDir, name)
			if jerr != nil {
				h.notifyWithCardUpdate(chatID, messageID, "error", "发送失败", jerr.Error())
				return
			}
			h.emitSendFile(chatID, absRoot, target, messageID)
			return
		default:
			h.notifyWithCardUpdate(chatID, messageID, "warning", "发送取消", "未识别的选择。")
			return
		}
	}
}

// emitSendFile reads one file into a TypeFile payload (shared helper) and
// ships it to the frontend, which uploads + sends. updateMessageID (the picker
// card) lets the frontend PATCH that card with the outcome. absRoot is
// re-enforced inside ReadFilePayload at read time.
func (h *Handler) emitSendFile(chatID, absRoot, path, updateMessageID string) {
	fileName := filepath.Base(path)
	payload, err := bridgebase.ReadFilePayload(chatID, fileName, absRoot, path, updateMessageID)
	if err != nil {
		h.notifyWithPromptID(chatID, "", "error", "发送失败", err.Error())
		return
	}
	h.sendCtrl(&protocol.Control{
		Type:   protocol.TypeFile,
		ChatID: chatID,
		File:   payload,
	})
}

// sendParentDirMini moves up one level without escaping absRoot. A local copy
// of bridgebase's helper since miniagent stays independent of the Core-based
// package's internals.
func sendParentDirMini(currDir, absRoot string) string {
	parent := filepath.Dir(currDir)
	rel, err := filepath.Rel(absRoot, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return absRoot
	}
	return parent
}
