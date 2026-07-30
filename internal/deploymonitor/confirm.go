package deploymonitor

import (
	"context"
	"fmt"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

// This file holds the /deploy-force confirmation gate: a destructive deploy
// (ARGS=--force skips deploy.sh's safety checks) must not fire on a single
// click. confirmAndDeployForce emits a permission card; awaitForceConfirm
// blocks on the AnswerBroker for the user's click, then runs the deploy or
// cancels. Kept out of handler.go so that file holds only the prompt dispatch
// + job-lifecycle state machine.

// confirmAndDeploy emits a confirmation card for /deploy (full deploy) and runs
// the deploy only after the user clicks "确认部署". The card binds the
// triggering promptID so the frontend patches the command's progress card in
// place; requestID = promptID (unique per message) is what the AnswerBroker
// routes the click to and what the frontend dedups double-clicks on. The wait
// runs on a goroutine so the SSE loop stays free; cancel/timeout emits a
// terminal notice bound to promptID.
func (h *Handler) confirmAndDeploy(ctx context.Context, chatID, promptID, cardMsgID string) error {
	requestID := promptID
	ch, ok := h.answers.Register(requestID)
	if !ok {
		// A confirm for this promptID is already pending (rapid double-send
		// before the first resolved); surface it instead of dropping silently.
		return h.notify(ctx, chatID, promptID, cardMsgID, "warning", "等待确认",
			"已有一次部署确认等待你的回应，请先完成或等待其失效。")
	}
	if err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypePermission,
		PromptID: promptID,
		ChatID:   chatID,
		Permission: &protocol.PermissionPayload{
			RequestID:        requestID,
			PromptID:         promptID,
			TakeOverProgress: true,
			Message:          "即将执行全量部署（feishu / claude / opencode / omp / miniagent），确认继续？",
			Options: []protocol.PermissionOption{
				{Label: "确认部署", Value: "confirm"},
				{Label: "取消", Value: "cancel"},
			},
		},
	}); err != nil {
		h.answers.Cancel(requestID)
		return err
	}
	go h.awaitDeploy(chatID, promptID, cardMsgID, ch) //nolint:gosec // G118: the wait must outlive the triggering request's ctx
	return nil
}

// awaitDeploy blocks on the confirm card's answer, then runs the deploy or
// cancels. On confirm it delegates to acquireAndRun (which takes the
// single-flight slot, emits the banner, and ships the terminal notice — all
// bound to promptID + cardMsgID). The confirm card's own messageID is
// distinct from the progress card's; after the click the confirm card stays
// in its "submitted" state (the deploy notice lands on the progress card, not
// the confirm card). Cancel/timeout releases the wait with a bound terminal
// notice.
func (h *Handler) awaitDeploy(chatID, promptID, cardMsgID string, ch <-chan *protocol.AnswerPayload) {
	confirmCtx, cancel := context.WithTimeout(context.Background(), confirmTimeout)
	defer cancel()
	select {
	case ans, ok := <-ch:
		if !ok || ans == nil || ans.Choice != "confirm" {
			h.notifyWithRetry(chatID, promptID, cardMsgID, "info", "已取消", "部署已取消，未执行任何操作。")
			return
		}
		if err := h.acquireAndRun(chatID, promptID, cardMsgID, "make", h.deployArgs(false, nil), "部署"); err != nil {
			h.notifyWithRetry(chatID, promptID, cardMsgID, "error", "部署失败", "启动部署失败："+err.Error())
		}
	case <-confirmCtx.Done():
		h.answers.Cancel(promptID)
		h.notifyWithRetry(chatID, promptID, cardMsgID, "warning", "确认超时",
			fmt.Sprintf("部署确认超过 %d 分钟未响应，已自动取消。", int(confirmTimeout.Minutes())))
	}
}

// confirmAndDeployForce emits a confirmation card for /deploy-force and runs
// the deploy only after the user clicks "确认强制部署". The card binds the
// triggering promptID so the frontend patches the command's progress card in
// place; requestID = promptID (unique per message) is what the AnswerBroker
// routes the click to and what the frontend dedups double-clicks on. The wait
// runs on a goroutine so the SSE loop stays free; cancel/timeout emits a
// terminal notice bound to promptID.
func (h *Handler) confirmAndDeployForce(ctx context.Context, chatID, promptID, cardMsgID string) error {
	requestID := promptID
	ch, ok := h.answers.Register(requestID)
	if !ok {
		// A confirm for this promptID is already pending (rapid double-send
		// before the first resolved); surface it instead of dropping silently.
		return h.notify(ctx, chatID, promptID, cardMsgID, "warning", "等待确认",
			"已有一次强制部署确认等待你的回应，请先完成或等待其失效。")
	}
	if err := h.rpc.SendControl(ctx, &protocol.Control{
		Type:     protocol.TypePermission,
		PromptID: promptID,
		ChatID:   chatID,
		Permission: &protocol.PermissionPayload{
			RequestID:        requestID,
			PromptID:         promptID,
			TakeOverProgress: true,
			Message:          "强制部署将向 `make deploy` 传入 ARGS=--force，跳过部分安全检查。确认继续？",
			Options: []protocol.PermissionOption{
				{Label: "确认强制部署", Value: "confirm"},
				{Label: "取消", Value: "cancel"},
			},
		},
	}); err != nil {
		h.answers.Cancel(requestID)
		return err
	}
	go h.awaitForceConfirm(chatID, promptID, cardMsgID, ch) //nolint:gosec // G118: the wait must outlive the triggering request's ctx
	return nil
}

// awaitForceConfirm blocks on the confirm card's answer, then runs the deploy
// or cancels. On confirm it delegates to acquireAndRun (which takes the
// single-flight slot, emits the banner, and ships the terminal notice — all
// bound to promptID + cardMsgID). The confirm card's own messageID is
// distinct from the progress card's; after the click the confirm card stays
// in its "submitted" state (the deploy notice lands on the progress card, not
// the confirm card). Cancel/timeout releases the wait with a bound terminal
// notice.
func (h *Handler) awaitForceConfirm(chatID, promptID, cardMsgID string, ch <-chan *protocol.AnswerPayload) {
	confirmCtx, cancel := context.WithTimeout(context.Background(), confirmTimeout)
	defer cancel()
	select {
	case ans, ok := <-ch:
		if !ok || ans == nil || ans.Choice != "confirm" {
			h.notifyWithRetry(chatID, promptID, cardMsgID, "info", "已取消", "强制部署已取消，未执行任何操作。")
			return
		}
		if err := h.acquireAndRun(chatID, promptID, cardMsgID, "make", h.deployArgs(true, nil), "部署"); err != nil {
			h.notifyWithRetry(chatID, promptID, cardMsgID, "error", "部署失败", "启动部署失败："+err.Error())
		}
	case <-confirmCtx.Done():
		h.answers.Cancel(promptID)
		h.notifyWithRetry(chatID, promptID, cardMsgID, "warning", "确认超时",
			fmt.Sprintf("强制部署确认超过 %d 分钟未响应，已自动取消。", int(confirmTimeout.Minutes())))
	}
}
