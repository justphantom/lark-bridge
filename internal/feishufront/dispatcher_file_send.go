package feishufront

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"runtime/debug"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// fileSendSem bounds concurrent file deliveries. Decode + upload + send of a
// 30 MiB file can take a minute and hold ~100 MB; an unbounded burst of /send
// controls would otherwise pile goroutines and memory onto the frontend.
var fileSendSem = make(chan struct{}, 4)

// dispatchFileAsync runs a TypeFile control off the caller's goroutine. The
// control pump is a single serial goroutine — a synchronous deliverFile would
// head-of-line block every chat's card updates behind one slow upload.
//
// The semaphore is acquired BEFORE spawning (non-blocking). Acquiring inside
// the goroutine bounded concurrency but not the queue: a buggy/compromised
// backend POSTing a burst of TypeFile controls would stack N goroutines, each
// pinning the full ctrl (base64 payload up to ~40 MB, decoded data another
// copy) — N queued ≈ N×90 MB resident, an OOM path. A blocking acquire here
// would itself head-of-line stall the serial control pump, so on saturation
// we fall back to a synchronous busy-notice on the picker card (no spawn →
// the ctrl is eligible for GC on return, no lingering 40 MB pin) instead of
// dropping the user-initiated send silently.
func (d *Dispatcher) dispatchFileAsync(ctx context.Context, ctrl *protocol.Control, backendType string) {
	select {
	case fileSendSem <- struct{}{}:
	default:
		d.reflectFileOutcome(ctx, ctrl, backendType, "error", "发送失败",
			"文件发送队列已满（并发上限 4），请稍后重试。")
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if l := d.logger.Load(); l != nil {
					l.Error("panic in file delivery",
						log.FieldChatID, ctrl.ChatID,
						log.FieldPanic, r,
						log.FieldStack, string(debug.Stack()))
				}
			}
			<-fileSendSem
		}()
		if err := d.handleFileControl(ctx, ctrl, backendType); err != nil {
			if l := d.logger.Load(); l != nil {
				l.Error("file control", log.FieldChatID, ctrl.ChatID, log.FieldError, err)
			}
		}
	}()
}

// handleFileControl materialises a TypeFile control: it base64-decodes the
// payload, asks the wired FileSender to upload+send the file into the chat,
// then reflects the outcome on the picker card the user clicked (when the
// payload carries its UpdateMessageID) or as a standalone notice. Errors are
// never dropped: a send failure always reaches the user. The handler always
// returns nil once the outcome has been reflected (the caller treats TypeFile
// as handled); a failure is a user-facing notice, not a dispatcher error.
//
// The decode/send error handling lives in deliverFile (no return value) so the
// `err != nil → reflect → return` branches never read as "returned nil despite
// an error" to the nilerr linter — the outcome IS the handler's terminal state.
func (d *Dispatcher) handleFileControl(ctx context.Context, ctrl *protocol.Control, backendType string) error {
	p := ctrl.File
	if p == nil {
		return fmt.Errorf("dispatcher: file control without payload")
	}
	if d.fileSender == nil {
		l := d.logger.Load()
		if l != nil {
			l.Warn("file control received but no FileSender wired",
				log.FieldChatID, ctrl.ChatID, "file_name", p.FileName)
		}
		d.reflectFileOutcome(ctx, ctrl, backendType, "error", "发送失败", "文件发送未配置（前端缺少 FileSender）。")
		return nil
	}
	d.deliverFile(ctx, ctrl, backendType)
	return nil
}

// deliverFile decodes the payload, sends the file via the wired FileSender,
// and reflects success or failure on the picker card (or a standalone notice).
// Split out of handleFileControl with no return value so its error branches
// (`err != nil → reflect → return`) are not flagged by nilerr.
func (d *Dispatcher) deliverFile(ctx context.Context, ctrl *protocol.Control, backendType string) {
	p := ctrl.File
	data, err := base64.StdEncoding.DecodeString(p.Content)
	if err != nil {
		d.reflectFileOutcome(ctx, ctrl, backendType, "error", "发送失败", "文件内容解码失败："+err.Error())
		return
	}
	if err := d.fileSender.SendFile(ctx, ctrl.ChatID, p.FileName, bytes.NewReader(data)); err != nil {
		l := d.logger.Load()
		if l != nil {
			l.Warn("send file failed",
				log.FieldChatID, ctrl.ChatID, "file_name", p.FileName, log.FieldError, err.Error())
		}
		d.reflectFileOutcome(ctx, ctrl, backendType, "error", "发送失败", "上传或发送失败："+err.Error())
		return
	}
	// Success: the file landing in the chat is the primary cue. Only flip the
	// picker card to green when we have one to patch; a direct /send <path>
	// (no picker) stays quiet to avoid a redundant notice.
	if p.UpdateMessageID != "" {
		d.reflectFileOutcome(ctx, ctrl, backendType, "success", "已发送", "已发送："+p.FileName)
	}
}

// reflectFileOutcome PATCHes the picker card (UpdateMessageID) with the
// outcome, falling back to a fresh standalone notice when there is no card to
// patch or the prior card was withdrawn. A withdrawn picker card on success is
// silently dropped (the file already arrived); on failure it re-sends so the
// error is never lost.
func (d *Dispatcher) reflectFileOutcome(ctx context.Context, ctrl *protocol.Control, backendType, level, title, msg string) {
	footer := cardkit.FooterInfo{BackendType: backendType, Status: title}
	card, err := cardkit.Notice(footer, level, title, msg, "", "", "")
	if err != nil {
		l := d.logger.Load()
		if l != nil {
			l.Warn("file outcome card render failed", log.FieldError, err.Error())
		}
		return
	}
	updateID := ""
	if p := ctrl.File; p != nil {
		updateID = p.UpdateMessageID
	}
	if updateID != "" {
		// Terminal frame: unconditionally drop any interactive binding
		// (cached bytes, TTL timer) still pointing at the picker card. A
		// submitted picker arms a delayed fallback PATCH (schedule
		// SubmitFallback) that re-sends the grey "已提交" bytes; the
		// fallback's guard skips only once the binding is gone. Without
		// this the fallback overwrites the green outcome — the bounce-back.
		d.evictInteractiveByMessageID(updateID, "")
		// Verified: the outcome PATCH can land inside Feishu's click
		// window (the user just clicked the picker) and get silently
		// reverted; read-back verification re-PATCHes if so.
		err = d.bot.UpdateCardVerified(ctx, updateID, d.interactiveCardID(updateID), card)
		if err == nil {
			// Outcome landed: mark terminal so a delayed emitSelectedCard
			// ("已选择 X") PATCH still sleeping out the click window is dropped
			// instead of reverting this frame — the bounce-back fix.
			d.markCardTerminal(updateID)
			return
		}
		// Card withdrawn on success: drop silently (the file already arrived).
		if feishu.IsCardGone(err) && level == "success" {
			return
		}
		// Otherwise — withdrawn card on failure, or any transient patch error
		// — fall through to a fresh card so the outcome is never lost.
	}
	if _, err := d.bot.SendCard(ctx, ctrl.ChatID, card, ""); err != nil {
		if l := d.logger.Load(); l != nil {
			l.Warn("file outcome fallback notice failed",
				log.FieldChatID, ctrl.ChatID, log.FieldError, err.Error())
		}
	}
}
