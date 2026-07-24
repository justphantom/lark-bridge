package miniagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// askWaitTimeout bounds how long a picker waits for a human answer. Feishu
// interactive cards expire on the frontend; this only needs to outlast that.
const askWaitTimeout = 9 * time.Minute

// answerBroker routes an interactive card's answer back to the goroutine that
// emitted the TypeQuestion control. It is a minimal local copy of
// bridgebase.AnswerBroker so miniagent stays independent of bridgebase.
type answerBroker struct {
	mu      sync.Mutex
	pending map[string]chan *protocol.AnswerPayload
}

func newAnswerBroker() *answerBroker {
	return &answerBroker{pending: make(map[string]chan *protocol.AnswerPayload)}
}

// Register reserves a slot for requestID and returns the channel that will
// receive the answer. Fails if requestID is already pending.
func (b *answerBroker) Register(requestID string) (<-chan *protocol.AnswerPayload, bool) {
	ch := make(chan *protocol.AnswerPayload, 1)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.pending[requestID]; exists {
		return nil, false
	}
	b.pending[requestID] = ch
	return ch, true
}

// Cancel removes a pending slot without delivering.
func (b *answerBroker) Cancel(requestID string) {
	b.mu.Lock()
	delete(b.pending, requestID)
	b.mu.Unlock()
}

// Deliver routes an inbound answer to the waiting goroutine.
func (b *answerBroker) Deliver(requestID string, ans *protocol.AnswerPayload) bool {
	b.mu.Lock()
	ch, ok := b.pending[requestID]
	if ok {
		delete(b.pending, requestID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- ans:
	default:
	}
	return true
}

// Drain closes every pending slot so blocked pickers return immediately.
// Called by Handler.Close.
func (b *answerBroker) Drain() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.pending {
		close(ch)
		delete(b.pending, id)
	}
}

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
		choice := pickAnswerValue(ans)
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

// pickAnswerValue extracts the user's selection. Custom input wins over a
// listed pick; Choices[0] carries a single-select's value.
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

func newRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q-" + hex.EncodeToString(b[:]), nil
}
