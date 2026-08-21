package miniagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// askWaitTimeout bounds how long a picker waits for a human answer. Feishu
// interactive cards expire on the frontend; this only needs to outlast that.
const askWaitTimeout = 9 * time.Minute

// The AnswerBroker (answer.go) is a miniagent-local type: the bridgebase
// shared layer dissolved into this package in the 2026-08-19 merge. Local
// helper functions below (pickAnswerValue / newRequestID) wrap its API for
// the picker flow.

// askAndWait emits a TypeQuestion card with the given options, then blocks
// until the user answers (via the frontend card → TypeAnswer event → broker
// Deliver) or askWaitTimeout elapses. Returns the selected value and the
// Feishu messageID of the card the user clicked (so the caller can patch it
// in place with the result rather than emitting a standalone notice).
//
// promptID is the command's triggering message ID: when non-empty the card
// carries PromptID + TakeOverProgress so the frontend morphs the command's
// progress card into the picker card (one card end-to-end); empty falls back
// to a standalone picker card. h provides the rpc (to emit the card), the
// answer broker, and the process-lifetime ctx. chatID scopes the card; label
// is the card's question label; options are the selectable items.
func (h *Handler) askAndWait(ctx context.Context, chatID, promptID, label string, options []string) (string, string, error) {
	if len(options) == 0 {
		return "", "", fmt.Errorf("没有可选项")
	}
	requestID, err := newRequestID()
	if err != nil {
		return "", "", fmt.Errorf("生成请求 ID 失败：%w", err)
	}
	ch, ok := h.answers.Register(requestID)
	if !ok {
		return "", "", fmt.Errorf("已有一个进行中的选择，请先完成或等待其失效")
	}

	ctrl := &protocol.Control{
		Type:     protocol.TypeQuestion,
		ChatID:   chatID,
		PromptID: promptID,
		Question: &protocol.QuestionPayload{
			RequestID: requestID,
			Questions: []protocol.QuestionItem{{
				Label:   label,
				Options: options,
			}},
			// promptID non-empty + TakeOverProgress lets the frontend morph
			// the command's progress card into the picker card, so the
			// whole /model (or /cd) flow lives on one card. Empty promptID
			// falls back to a standalone picker card.
			TakeOverProgress: promptID != "",
		},
	}
	h.sendCtrl(ctrl)
	// sendCtrl logs failures internally; we proceed to wait regardless —
	// a delayed card still works if the emit retried successfully.

	waitCtx, waitCancel := context.WithTimeout(ctx, askWaitTimeout)
	defer waitCancel()
	select {
	case ans, ok := <-ch:
		if !ok {
			return "", "", fmt.Errorf("服务正在关闭，请稍后重试")
		}
		choice := PickAnswerValue(ans)
		if choice == "" {
			return "", "", fmt.Errorf("未选择任何%s", label)
		}
		messageID := ""
		if ans != nil {
			messageID = ans.MessageID
		}
		return choice, messageID, nil
	case <-waitCtx.Done():
		h.answers.Cancel(requestID)
		return "", "", fmt.Errorf("选择超时（>%s），请重新发起", askWaitTimeout)
	}
}

// askCardUpdate is miniagent's in-place counterpart to askAndWait for the
// second and later rounds of a multi-round picker (/send's directory browser):
// it PATCHes updateMessageID (the round-1 card) with a fresh option list +
// requestID instead of morphing the progress card or shipping a standalone
// card. Blocks for the answer like askAndWait.
func (h *Handler) askCardUpdate(ctx context.Context, chatID, updateMessageID, label string, options []string) (string, string, error) {
	if len(options) == 0 {
		return "", "", fmt.Errorf("没有可选项")
	}
	requestID, err := newRequestID()
	if err != nil {
		return "", "", fmt.Errorf("生成请求 ID 失败：%w", err)
	}
	ch, ok := h.answers.Register(requestID)
	if !ok {
		return "", "", fmt.Errorf("已有一个进行中的选择，请先完成或等待其失效")
	}
	ctrl := &protocol.Control{
		Type:   protocol.TypeQuestion,
		ChatID: chatID,
		Question: &protocol.QuestionPayload{
			RequestID:       requestID,
			Questions:       []protocol.QuestionItem{{Label: label, Options: options}},
			UpdateMessageID: updateMessageID,
		},
	}
	h.sendCtrl(ctrl)

	waitCtx, waitCancel := context.WithTimeout(ctx, askWaitTimeout)
	defer waitCancel()
	select {
	case ans, ok := <-ch:
		if !ok {
			return "", "", fmt.Errorf("服务正在关闭，请稍后重试")
		}
		choice := PickAnswerValue(ans)
		if choice == "" {
			return "", "", fmt.Errorf("未选择任何%s", label)
		}
		messageID := updateMessageID
		if ans != nil && ans.MessageID != "" {
			messageID = ans.MessageID
		}
		return choice, messageID, nil
	case <-waitCtx.Done():
		h.answers.Cancel(requestID)
		return "", "", fmt.Errorf("选择超时（>%s），请重新发起", askWaitTimeout)
	}
}

func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q-" + hex.EncodeToString(b[:]), nil
}
