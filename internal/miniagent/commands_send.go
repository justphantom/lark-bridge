package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// cmdSend delivers a file from the chat's active working directory into the
// chat via the frontend (send-file-design.md). No-arg opens the directory
// browser; a relative path sends directly. miniagent has no Core
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
	absRoot, err := ResolveRoot(dir)
	if err != nil {
		return "error", "发送失败", "工作目录无效：" + err.Error()
	}
	promptID := h.PromptIDForPickers(chatID)
	if arg != "" {
		target, jerr := SafeJoin(absRoot, arg)
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
//
// Round 1 morphs the progress card; every later round PATCHes that SAME card
// in place via askCardUpdate so descending into a directory updates the picker
// rather than leaving the prior card behind and piling up a new card per
// level. pickerMsgID carries the round-1 card's message_id across iterations.
func (h *Handler) runSendBrowser(chatID, promptID, absRoot string) {
	currDir := absRoot
	pickerMsgID := ""
	for {
		entries, err := os.ReadDir(currDir)
		if err != nil {
			h.notifyWithPromptID(chatID, promptID, "error", "发送失败", "读取目录失败："+err.Error())
			return
		}
		options := BuildSendOptions(currDir, absRoot, entries)
		if len(options) == 0 {
			h.notifyWithPromptID(chatID, promptID, "warning", "发送文件", "当前目录为空。")
			return
		}
		label := "选择要发送的文件（📁 进入子目录，⬆️ 返回上级）"
		var (
			choice    string
			messageID string
			aerr      error
		)
		if pickerMsgID == "" {
			choice, messageID, aerr = h.askAndWait(context.Background(), chatID, promptID, label, options)
		} else {
			choice, messageID, aerr = h.askCardUpdate(context.Background(), chatID, pickerMsgID, label, options)
		}
		if aerr != nil {
			h.notifyWithPromptID(chatID, promptID, "warning", "发送取消", aerr.Error())
			return
		}
		if pickerMsgID == "" {
			pickerMsgID = messageID
		}
		kind, name := ParseSendOption(choice)
		switch kind {
		case "up":
			currDir = sendParentDirMini(currDir, absRoot)
		case "dir":
			// SafeJoin per transition so a symlinked (or mid-browse swapped)
			// directory cannot walk the browser outside absRoot.
			target, jerr := SafeJoin(currDir, name)
			if jerr != nil {
				h.notifyWithPromptID(chatID, promptID, "error", "发送失败", jerr.Error())
				return
			}
			currDir = target
		case "file":
			target, jerr := SafeJoin(currDir, name)
			if jerr != nil {
				h.notifyWithCardUpdate(chatID, messageID, "error", "发送失败", jerr.Error())
				return
			}
			// Same selected-state lock as the picker flow: terminates the
			// interactive binding before the upload so no fallback can race the
			// outcome card.
			h.emitSelectedCard(chatID, messageID, "已选择 "+name)
			h.emitSendFile(chatID, absRoot, target, messageID)
			return
		default:
			h.notifyWithCardUpdate(chatID, messageID, "warning", "发送取消", "未识别的选择。")
			return
		}
	}
}

// emitSelectedCard PATCHes the picker card into its final locked "user picked
// X" state: a single-option question card (already selected) with a fresh,
// never registered requestID. The frontend ends any prior interactive binding on
// this card when it registers the refresh, so the delayed submit-fallback
// PATCH from the just-clicked round never fires and cannot race the outcome
// card. Best-effort: a failure leaves the prior round's card, which the
// outcome still patches.
func (h *Handler) emitSelectedCard(chatID, updateMessageID, label string) {
	if updateMessageID == "" {
		return
	}
	requestID, err := newRequestID()
	if err != nil {
		return
	}
	h.sendCtrl(&protocol.Control{
		Type:   protocol.TypeQuestion,
		ChatID: chatID,
		Question: &protocol.QuestionPayload{
			RequestID:       requestID,
			Questions:       []protocol.QuestionItem{{Label: label, Options: []string{label}}},
			UpdateMessageID: updateMessageID,
		},
	})
}

// emitSendFile reads one file into a TypeFile payload (shared helper) and
// ships it to the frontend, which uploads + sends. updateMessageID (the picker
// card) lets the frontend PATCH that card with the outcome. absRoot is
// re-enforced inside ReadFilePayload at read time.
func (h *Handler) emitSendFile(chatID, absRoot, path, updateMessageID string) {
	fileName := filepath.Base(path)
	payload, err := ReadFilePayload(chatID, fileName, absRoot, path, updateMessageID)
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

// sendParentDirMini moves up one level without escaping absRoot.
func sendParentDirMini(currDir, absRoot string) string {
	parent := filepath.Dir(currDir)
	rel, err := filepath.Rel(absRoot, parent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return absRoot
	}
	return parent
}
