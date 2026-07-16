package goosebridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/hu/lark-bridge/internal/protocol"
)

// emitNoticeTimeout bounds each Notice emit an interactive picker makes on its
// own (outside the dispatcher). Long enough to ride out a transient IPC blip,
// short enough that a stuck POST does not pin a goroutine.
const emitNoticeTimeout = 10 * time.Second

// askAndWait runs the interactive-selection loop for a setting picker: emits a
// Question card offering options (plus an optional custom-input box), then
// blocks until the user answers or askWaitTimeout elapses.
//
// Unlike opencode-back's variant there is no listFn: goose's model/
// effort options are static config values, so there is no CLI fork and no
// 90s listFnTimeout. The Question emit and the answer wait both derive their
// ctx from h.appCtx (NOT the dispatcher's 15s ctx) because waiting for a human
// answer is unbounded in practice. The caller MUST run in the dispatcher's
// goSafe goroutine, which tolerates the block without stalling SSE.
//
// allowCustom controls whether the card shows a free-text input box alongside
// the select. /model allows custom (any model name); /perm and /effort do not
// (they restrict to the listed, validated values).
//
// Returns the chosen value (custom input takes priority over a listed pick),
// or an error describing why no answer was obtained.
func (h *Handler) askAndWait(chatID, label string, options []string, allowCustom bool) (string, error) {
	if len(options) == 0 {
		return "", fmt.Errorf("没有可用的选项")
	}

	requestID, err := newRequestID()
	if err != nil {
		return "", fmt.Errorf("生成请求 ID 失败：%w", err)
	}
	ch, ok := h.registerAnswer(requestID)
	if !ok {
		return "", fmt.Errorf("已有一个进行中的选择，请先完成或等待其失效")
	}

	q := &protocol.Control{
		Type:   protocol.TypeQuestion,
		ChatID: chatID,
		Question: &protocol.QuestionPayload{
			RequestID: requestID,
			Questions: []protocol.QuestionItem{{
				Label:   label,
				Options: options,
				Custom:  allowCustom,
			}},
		},
	}
	emitCtx, emitCancel := context.WithTimeout(h.appCtx, emitNoticeTimeout)
	defer emitCancel()
	if err := h.emit(emitCtx, "", q); err != nil {
		h.cancelAnswer(requestID)
		return "", fmt.Errorf("发送选择卡片失败：%w", err)
	}

	// Waiting for a human answer is unbounded; derive a fresh deadline from
	// the process-lifetime appCtx so the dispatcher's 15s ctx does not cut it
	// short.
	waitCtx, waitCancel := context.WithTimeout(h.appCtx, askWaitTimeout)
	defer waitCancel()

	select {
	case ans, ok := <-ch:
		if !ok {
			// Channel closed by drainAnswers during shutdown.
			return "", errors.New("服务正在关闭，请稍后重试")
		}
		choice := pickAnswerValue(ans)
		if choice == "" {
			return "", errors.New("未选择任何选项")
		}
		return choice, nil
	case <-waitCtx.Done():
		h.cancelAnswer(requestID)
		if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("选择超时（>%s），请重新发起", askWaitTimeout)
		}
		return "", errors.New("等待选择被中断")
	}
}

// pickAnswerValue extracts the user's selection from an AnswerPayload. A
// custom-typed value wins over a listed pick (the user explicitly overrode
// the list); the Choices slice carries a single-select's value at index 0.
func pickAnswerValue(ans *protocol.AnswerPayload) string {
	if ans == nil {
		return ""
	}
	if ans.Custom != "" {
		return ans.Custom
	}
	if len(ans.Choices) > 0 {
		return ans.Choices[0]
	}
	return ""
}

// newRequestID returns a random hex string for an interactive card's
// requestID. crypto/rand keeps it unguessable so a stale card click after a
// timeout cannot collide with a fresh picker.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q-" + hex.EncodeToString(b[:]), nil
}

// emitNotice sends a Notice control on the picker's own lifecycle. Interactive
// pickers return Handled=true and bypass the dispatcher, so they cannot reuse
// the dispatcher's ctx (which may expire during the wait). This helper derives
// a fresh ctx from h.appCtx.
func (h *Handler) emitNotice(chatID, level, title, body string, extra ...string) error {
	ctx, cancel := context.WithTimeout(h.appCtx, emitNoticeTimeout)
	defer cancel()
	np := &protocol.NoticePayload{Level: level, Title: title, Message: body}
	// extra carries optional Field/Before/After in that order, matching the
	// ChangeResult shape the renderer expects for a before→after block.
	if len(extra) > 0 {
		np.Field = extra[0]
	}
	if len(extra) > 1 {
		np.Before = extra[1]
	}
	if len(extra) > 2 {
		np.After = extra[2]
	}
	return h.emit(ctx, "", &protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: np,
	})
}
