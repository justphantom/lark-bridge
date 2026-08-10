package agnesback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// modelKeepPrefix prefixes the first option of each /model picker question:
// picking it leaves the slot's model unchanged. The current effective model is
// embedded so the card itself shows the status quo.
const modelKeepPrefix = "保持不变（当前："

// modelSlots lists the three slots in picker-card question order. Choices[i]
// in the submitted answer corresponds to ModelSlots[i].
var modelSlots = []struct {
	slot  string
	label string
}{
	{ModelSlotChat, "提示词模型（chat）"},
	{ModelSlotImage, "图片模型（image）"},
	{ModelSlotVideo, "视频模型（video）"},
}

// pickerWaitTimeout bounds how long a /model picker waits for the user's
// answer. It only needs to outlast the frontend's interactive-card expiry.
const pickerWaitTimeout = 9 * time.Minute

// modelOptionsFor returns the picker's option list for one slot: a
// "keep current" first entry followed by the configured model list.
func (h *Handler) modelOptionsFor(slot string) []string {
	eff, _ := h.effectiveModels()
	opts := []string{modelKeepPrefix + eff[slot] + "）"}
	var list []string
	switch slot {
	case ModelSlotChat:
		list = h.cfg.ChatModels
	case ModelSlotImage:
		list = h.cfg.ImageModels
	case ModelSlotVideo:
		list = h.cfg.VideoModels
	}
	return append(opts, list...)
}

// runModelPicker emits a single question card with three questions (one per
// model slot), blocks for the user's answer, applies every changed slot, and
// patches the card in place with the result. Must run on a background
// goroutine: it blocks up to pickerWaitTimeout.
func (h *Handler) runModelPicker(chatID, promptID string) {
	items := make([]protocol.QuestionItem, 0, len(modelSlots))
	for _, s := range modelSlots {
		items = append(items, protocol.QuestionItem{
			Label:   s.label,
			Options: h.modelOptionsFor(s.slot),
		})
	}

	requestID, err := newPickerRequestID()
	if err != nil {
		// Picker flow: notify failure can only be logged — no card to patch.
		if nerr := h.notify(context.Background(), chatID, promptID, "", "error", "选择失败", "生成请求 ID 失败："+err.Error()); nerr != nil {
			h.logger.Warn("agnes: picker requestID-failure notice not delivered", "error", nerr)
		}
		return
	}
	ch, ok := h.answers.Register(requestID)
	if !ok {
		if nerr := h.notify(context.Background(), chatID, promptID, "", "error", "选择失败", "生成请求 ID 冲突，请重试"); nerr != nil {
			h.logger.Warn("agnes: picker register-conflict notice not delivered", "error", nerr)
		}
		return
	}

	h.sendCtrl(&protocol.Control{
		Type:     protocol.TypeQuestion,
		ChatID:   chatID,
		PromptID: promptID,
		Question: &protocol.QuestionPayload{
			RequestID:        requestID,
			Questions:        items,
			TakeOverProgress: promptID != "",
		},
	})

	waitCtx, cancel := context.WithTimeout(context.Background(), pickerWaitTimeout)
	defer cancel()
	var ans *protocol.AnswerPayload
	select {
	case a, open := <-ch:
		if !open {
			return
		}
		ans = a
	case <-waitCtx.Done():
		h.answers.Cancel(requestID)
		if nerr := h.notify(context.Background(), chatID, promptID, "", "warning", "选择超时",
			fmt.Sprintf("模型选择超时（>%s），请重新发送 /model。", pickerWaitTimeout)); nerr != nil {
			h.logger.Warn("agnes: picker timeout notice not delivered", "error", nerr)
		}
		return
	}

	eff, overridden := h.effectiveModels()
	var changes []string
	for i, s := range modelSlots {
		if i >= len(ans.Choices) {
			break
		}
		choice := ans.Choices[i]
		if choice == "" || strings.HasPrefix(choice, modelKeepPrefix) {
			continue
		}
		old := eff[s.slot]
		h.setModelOverride(s.slot, choice)
		changes = append(changes, fmt.Sprintf("- %s：%s → %s", s.slot, old, choice))
	}

	messageID := ans.MessageID
	var title, body string
	if len(changes) == 0 {
		title = "模型未变化"
		body = currentModelsBody(eff, overridden)
	} else {
		title = "已切换模型"
		body = strings.Join(changes, "\n") + "\n\n" + currentModelsBody(eff, overridden) +
			"\n\n进程级生效，重启后回落配置文件值。"
	}
	h.sendCtrl(&protocol.Control{
		Type:   protocol.TypeNotice,
		ChatID: chatID,
		Notice: &protocol.NoticePayload{
			Level:           "success",
			Title:           title,
			Message:         body,
			UpdateMessageID: messageID,
		},
	})
}

// currentModelsBody renders the effective models with override markers.
func currentModelsBody(eff map[string]string, overridden map[string]bool) string {
	mark := func(slot string) string {
		if overridden[slot] {
			return ""
		}
		return "（默认）"
	}
	return fmt.Sprintf("当前模型：chat=%s%s / image=%s%s / video=%s%s",
		eff[ModelSlotChat], mark(ModelSlotChat),
		eff[ModelSlotImage], mark(ModelSlotImage),
		eff[ModelSlotVideo], mark(ModelSlotVideo))
}

// newPickerRequestID returns a random picker request ID.
func newPickerRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "q-" + hex.EncodeToString(b[:]), nil
}
