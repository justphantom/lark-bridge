package opencodebridge

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

// listFnTimeout bounds the opencode models/agent list subcommand invoked by
// the picker. The CLI's heavy startup (provider/config load) makes these take
// 25–50s observed; 90s covers the worst seen while still bounding a hang.
const listFnTimeout = 90 * time.Second

// askAndWait runs the full interactive-selection loop for a setting picker:
// lists the available options via listFn, emits a Question card offering them
// (plus a custom-input box so a value not listed can still be typed), then
// blocks until the user answers or askWaitTimeout elapses.
//
// Both the listFn call and the answer wait derive their ctx from h.appCtx, NOT
// from any caller ctx: the opencode CLI takes 25–50s to list models/agents,
// far exceeding the dispatcher's cmdutil.Timeout (15s). Callers MUST run this
// in a background goroutine (the dispatcher returns immediately with a
// placeholder Notice and Handled=true). chatID satisfies protocol.Validate
// (Question controls require ChatID). kind/label tailor the card copy.
//
// Returns the chosen value (custom input takes priority over a listed pick),
// or an error describing why no answer was obtained.
func (h *Handler) askAndWait(
	chatID, replyToID, kind, label string,
	listFn func(context.Context) ([]string, error),
	allowCustom bool,
) (string, error) {
	// listTimeoutCtx bounds the (slow) list subcommand independently of any
	// caller deadline. It rides the process-lifetime appCtx so a shutdown
	// still cancels an in-flight fork.
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

	// Waiting for a human answer is unbounded in practice; derive a fresh
	// deadline from the process-lifetime appCtx.
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
// the dispatcher's ctx (which expired during the wait). This helper derives a
// fresh ctx from h.appCtx, letting a confirmation or error Notice land after
// a multi-minute wait.
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
