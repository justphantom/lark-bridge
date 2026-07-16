package peribridge

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
// own (outside the dispatcher).
const emitNoticeTimeout = 10 * time.Second

// listFnTimeout bounds a picker's list function. peri has no list subcommand,
// so the only caller (/cd) lists local filesystem dirs — fast — but the bound
// is retained for safety against a slow filesystem.
const listFnTimeout = 90 * time.Second

// askAndWait runs the full interactive-selection loop for a setting picker:
// lists the available options via listFn, emits a Question card offering them,
// then blocks until the user answers or askWaitTimeout elapses.
//
// Both the listFn call and the answer wait derive their ctx from h.appCtx, NOT
// from any caller ctx. Callers MUST run this in a background goroutine (the
// dispatcher returns immediately with a placeholder Notice and Handled=true).
//
// Returns the chosen value, or an error describing why no answer was obtained.
func (h *Handler) askAndWait(
	chatID, replyToID, kind, label string,
	listFn func(context.Context) ([]string, error),
	allowCustom bool,
) (string, error) {
	listCtx, listCancel := context.WithTimeout(h.appCtx, listFnTimeout)
	defer listCancel()
	options, err := listFn(listCtx)
	if err != nil {
		return "", fmt.Errorf("获取%s列表失败：%w", kind, err)
	}
	if len(options) == 0 {
		return "", fmt.Errorf("没有可用的%s", kind)
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
	if err := h.emit(emitCtx, replyToID, q); err != nil {
		h.cancelAnswer(requestID)
		return "", fmt.Errorf("发送选择卡片失败：%w", err)
	}

	waitCtx, waitCancel := context.WithTimeout(h.appCtx, askWaitTimeout)
	defer waitCancel()

	select {
	case ans, ok := <-ch:
		if !ok {
			return "", errors.New("服务正在关闭，请稍后重试")
		}
		choice := pickAnswerValue(ans)
		if choice == "" {
			return "", fmt.Errorf("未选择任何%s", kind)
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
// custom-typed value wins over a listed pick.
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
// requestID.
func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q-" + hex.EncodeToString(b[:]), nil
}

// emitNotice sends a Notice control on the picker's own lifecycle. Interactive
// pickers return Handled=true and bypass the dispatcher, so they cannot reuse
// the dispatcher's ctx (which expired during the wait).
func (h *Handler) emitNotice(chatID, level, title, body string, extra ...string) error {
	ctx, cancel := context.WithTimeout(h.appCtx, emitNoticeTimeout)
	defer cancel()
	np := &protocol.NoticePayload{Level: level, Title: title, Message: body}
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
