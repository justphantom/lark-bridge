package deploymonitor

import (
	"context"
	"fmt"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// This file holds the /deploy-some multi-select gate: unlike /deploy (one-shot
// full deploy) and /deploy-force (confirm gate on a full force deploy),
// /deploy-some emits a multi-select question card and runs the deploy only for
// the submitted subset, passing ARGS=--services=<csv> to deploy.sh. Kept out
// of handler.go so that file holds only the prompt dispatch + job-lifecycle
// state machine.

// deployServices is the fixed business-service short-name set, matching
// deploy.sh select_services (deploy.sh:98). Validated up front so an invalid
// pick fails fast here instead of after a wasted make run.
var deployServices = []string{"feishu", "claude", "opencode", "miniagent"}

func isKnownService(s string) bool {
	for _, k := range deployServices {
		if k == s {
			return true
		}
	}
	return false
}

// confirmAndDeploySome emits a multi-select question card for /deploy-some and
// runs the deploy only after the user submits a non-empty, valid subset. The
// card binds the triggering promptID so the frontend patches the command's
// progress card in place; requestID = promptID (unique per message) is what
// the AnswerBroker routes the submission to and what the frontend dedups
// double-submits on. The wait runs on a goroutine so the SSE loop stays free;
// cancel/timeout/empty/invalid all emit a terminal notice bound to promptID.
func (h *Handler) confirmAndDeploySome(ctx context.Context, chatID, promptID, cardMsgID string) error {
	requestID := promptID
	ch, ok := h.answers.Register(requestID)
	if !ok {
		// A picker for this promptID is already pending (rapid double-send
		// before the first resolved); surface it instead of dropping silently.
		return h.notify(ctx, chatID, promptID, cardMsgID, "warning", "等待选择",
			"已有一次部署服务选择等待你的回应，请先完成或等待其失效。")
	}
	if err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypeQuestion,
		PromptID: promptID,
		ChatID:   chatID,
		Question: &protocol.QuestionPayload{
			RequestID: requestID,
			PromptID:  promptID,
			Questions: []protocol.QuestionItem{{
				Label:    "选择本次部署的服务（可多选）",
				Options:  deployServices,
				Multiple: true,
			}},
		},
	}); err != nil {
		h.answers.Cancel(requestID)
		return err
	}
	go h.awaitDeploySome(chatID, promptID, cardMsgID, ch) //nolint:gosec // G118: the wait must outlive the triggering request's ctx
	return nil
}

// awaitDeploySome blocks on the multi-select card, validates the picks, then
// delegates to acquireAndRun on success. Empty selection or an unknown service
// name cancels with a bound terminal notice; timeout cancels the AnswerBroker
// slot. The picker card's own messageID is distinct from the progress card's;
// after the submit the picker card stays put and the deploy notice lands on
// the progress card (same shape as /deploy-force in confirm.go).
func (h *Handler) awaitDeploySome(chatID, promptID, cardMsgID string, ch <-chan *protocol.AnswerPayload) {
	selectCtx, cancel := context.WithTimeout(context.Background(), confirmTimeout)
	defer cancel()
	select {
	case ans, ok := <-ch:
		if !ok || ans == nil || len(ans.Choices) == 0 {
			h.notifyWithRetry(chatID, promptID, cardMsgID, "info", "已取消", "未选择任何服务，部署已取消。")
			return
		}
		for _, s := range ans.Choices {
			if !isKnownService(s) {
				h.notifyWithRetry(chatID, promptID, cardMsgID, "error", "选择无效",
					"包含未知服务："+s+"（有效：feishu claude opencode miniagent）")
				return
			}
		}
		if err := h.acquireAndRun(selectCtx, chatID, promptID, cardMsgID, "make", h.deployArgs(false, ans.Choices), "部署"); err != nil {
			h.notifyWithRetry(chatID, promptID, cardMsgID, "error", "部署失败", "启动部署失败："+err.Error())
		}
	case <-selectCtx.Done():
		h.answers.Cancel(promptID)
		h.notifyWithRetry(chatID, promptID, cardMsgID, "warning", "选择超时",
			fmt.Sprintf("部署服务选择超过 %d 分钟未响应，已自动取消。", int(confirmTimeout.Minutes())))
	}
}
