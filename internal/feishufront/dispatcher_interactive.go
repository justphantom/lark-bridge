package feishufront

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/feishufront/renderer"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// sendInteractive renders a permission-request or question card and ships it
// as a standalone card (its own messageID), leaving any in-flight progress
// card untouched. It binds requestID → messageID so a later card action can
// find it, caches the rendered card bytes for the submitted/expired/finalised
// state flips, and schedules the TTL expiry notice. Called by DispatchControl
// for TypeQuestion controls.
func (d *Dispatcher) sendInteractive(ctx context.Context, ctrl *protocol.Control, backendType string) error {
	_, _, chatID, footer := d.resolveFooter(ctrl, backendType)
	footer.Status = "待确认"
	header := cardkit.HeaderInfo{BackendType: backendType}
	card, err := renderer.RenderInteractive(ctrl, header, footer)
	if err != nil {
		return err
	}
	var requestID string
	if ctrl.Type == protocol.TypePermission {
		requestID = ctrl.Permission.RequestID
	} else {
		requestID = ctrl.Question.RequestID
	}
	messageID, err := d.sendInteractiveCard(ctx, ctrl, chatID, card)
	if err != nil {
		return err
	}
	if requestID != "" {
		// Evict expired interactive bindings (and their cached card bytes)
		// before adding the new one, so cards ignored by the user — or left
		// dangling when the backend crashes mid-answer — do not leak.
		//
		// Lock ordering: TurnManager.mu is always acquired (or its work completed)
		// before cardMu. SweepInteractive mutates TurnManager under its own lock;
		// we then drop the expired cached bytes under cardMu.
		expired := d.turns.SweepInteractive()
		d.cardMu.Lock()
		for _, rid := range expired {
			delete(d.cards, rid)
		}
		d.cardMu.Unlock()

		// An in-place picker refresh PATCHes a card that a prior round still
		// owns: drop that round's binding (and its stopped TTL timer / cached
		// bytes) so the new requestID owns the card and nothing leaks.
		if ctrl.Type == protocol.TypeQuestion && ctrl.Question != nil && ctrl.Question.UpdateMessageID != "" {
			d.evictInteractiveByMessageID(messageID, requestID)
		}

		// Record the new binding under TurnManager, then cache the card bytes
		// and schedule the TTL timer under cardMu.
		d.turns.BindInteractive(requestID, messageID, ctrl.PromptID)
		d.cardMu.Lock()
		d.cards[requestID] = card
		// Schedule the expiry notice; if the user never responds within the
		// TTL the card is flipped to a "已失效" state instead of waiting grey
		// forever. Stopped on submit (DispatchCardAction).
		reqID := requestID
		msgID := messageID
		d.interactiveTimers[requestID] = time.AfterFunc(cardkit.InteractiveTimeout, func() {
			goSafe(func() { d.expireInteractive(reqID, msgID) })
		})
		d.cardMu.Unlock()
	}
	return nil
}

// sendInteractiveCard ships an interactive card. A slash-command picker
// (TakeOverProgress set, e.g. /model) morphs the progress card the dispatcher
// opened for the command message into the picker card, keeping the whole
// command→pick→result interaction on one card; the turn is finished either
// way because its progress lifecycle ends with the takeover. Mid-turn
// permission/question cards (flag unset) ship standalone so the streaming
// progress card stays untouched. Falls back to a fresh card when no turn is
// open or the in-place update fails.
// interactiveTakeOver reports whether an interactive control (question or
// permission) wants to morph the progress card instead of shipping standalone.
func interactiveTakeOver(ctrl *protocol.Control) bool {
	if ctrl.Type == protocol.TypePermission {
		return ctrl.Permission != nil && ctrl.Permission.TakeOverProgress
	}
	return ctrl.Question != nil && ctrl.Question.TakeOverProgress
}

func (d *Dispatcher) sendInteractiveCard(ctx context.Context, ctrl *protocol.Control, chatID string, card []byte) (string, error) {
	// Multi-round picker refresh (the /send directory browser): PATCH the
	// existing picker card in place with the new option set instead of morphing
	// the progress card or shipping a standalone one. The click that triggered
	// this refresh is still inside Feishu's ~3-5s card.action.trigger handling
	// window, during which an immediate PatchMessage gets silently reverted —
	// so the PATCH is delayed past the window via a background goroutine
	// (same pattern as handleBackendChoice). The card binding/cache/timer are
	// wired by sendInteractive immediately so a click once the PATCH lands
	// resolves correctly; only the Feishu-side bytes lag by cardPatchDelay.
	if ctrl.Type == protocol.TypeQuestion && ctrl.Question != nil && ctrl.Question.UpdateMessageID != "" {
		msgID := ctrl.Question.UpdateMessageID
		delay := d.cardPatchDelay
		if delay <= 0 {
			delay = cardPatchDelayDefault
		}
		goSafe(func() {
			time.Sleep(delay)
			patchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeSendTimeout)
			defer cancel()
			if err := d.bot.UpdateCardVerified(patchCtx, msgID, card); err != nil {
				if l := d.logger.Load(); l != nil {
					l.Warn("delayed picker refresh UpdateCard failed",
						log.FieldMessageID, msgID, log.FieldError, err.Error())
				}
			}
		})
		return msgID, nil
	}
	if interactiveTakeOver(ctrl) && ctrl.PromptID != "" {
		if turn, ok := d.turns.Get(ctrl.PromptID); ok {
			// Flush pending debounced progress frames first so they cannot
			// land after the picker card and overwrite it.
			if d.debouncer != nil {
				d.debouncer.flush()
			}
			d.turns.Finish(ctrl.PromptID)
			d.cleanupProgress(ctrl.PromptID, "")
			if err := d.bot.UpdateCard(ctx, turn.MessageID, card); err == nil {
				return turn.MessageID, nil
			}
		}
	}
	return d.bot.SendCard(ctx, chatID, card, "")
}

// evictInteractiveByMessageID drops every interactive-card binding, cached
// card bytes and TTL timer pointing at messageID except keepRequestID. Called
// before binding the new round of an in-place picker refresh so the prior
// round's requestID does not linger against the same card (its timer was
// already stopped on submit, so without this the cache entry would leak until
// a SweepInteractive pass that never reaps a stopped-timer entry).
//
// Lock ordering: TurnManager state is removed first, then the paired cardMu
// state is cleaned up. This matches the rest of the interactive lifecycle and
// avoids lock-order inversion with sendInteractive.
func (d *Dispatcher) evictInteractiveByMessageID(messageID, keepRequestID string) {
	rids := d.turns.UnbindInteractiveByMessageID(messageID, keepRequestID)
	d.cardMu.Lock()
	for _, rid := range rids {
		if t := d.interactiveTimers[rid]; t != nil {
			t.Stop()
			delete(d.interactiveTimers, rid)
		}
		delete(d.cards, rid)
	}
	d.cardMu.Unlock()
}

// submitSummary renders the confirmation line prepended to a submitted card.
// A permission card carries the choice in value["choice"]; a question card's
// selections arrive in form_value (parsed into choices + custom). Both produce
// a "✓ 已回答: …" echo so the user sees what was picked at a glance.
func submitSummary(action *feishu.CardAction) string {
	if c, ok := action.Value["choice"].(string); ok && c != "" {
		return "✓ 你选择了「" + choiceLabel(c) + "」"
	}
	if len(action.FormValue) > 0 {
		choices, custom := parseQuestionFormValue(action.FormValue)
		if s := questionAnswerSummary(choices, custom); s != "" {
			return "✓ 已回答: " + s
		}
	}
	return "✓ 已提交，正在处理…"
}

// questionAnswerSummary picks the most meaningful answer text from parsed form
// values: a custom input wins over a listed selection (the user explicitly
// overrode the list).
func questionAnswerSummary(choices []string, custom string) string {
	if custom != "" {
		return custom
	}
	if len(choices) > 0 {
		return strings.Join(choices, "、")
	}
	return ""
}

// choiceLabel turns the machine choice value into the button label the user
// actually clicked, so the confirmation echo matches what was on screen.
// "allow"/"deny" come from allow/deny permission cards; "confirm"/"cancel"
// come from confirmation-style permission cards (e.g. /clean).
// The exact button Label (e.g. "确认删除") is not carried back in the action,
// so these map to the generic word; any unmapped value is returned verbatim
// rather than swallowed.
func choiceLabel(c string) string {
	switch c {
	case "allow":
		return "允许"
	case "deny":
		return "拒绝"
	case "confirm":
		return "确认"
	case "cancel":
		return "取消"
	}
	return c
}

// expireInteractive flips a still-pending interactive card to its expired
// state. Called by the TTL timer. If the user submitted in the meantime the
// binding is already gone (InteractiveMessageID returns false) and this is a
// no-op. cardMu serialises against a concurrent submit so the worst case is a
// benign overwrite, not a data race.
//
// Lock ordering: remove the TurnManager binding first, then clean up the
// paired cardMu state. This avoids lock-order inversion with sendInteractive.
func (d *Dispatcher) expireInteractive(requestID, messageID string) {
	d.turns.UnbindInteractive(requestID)
	d.cardMu.Lock()
	orig := d.cards[requestID]
	delete(d.cards, requestID)
	if t := d.interactiveTimers[requestID]; t != nil {
		t.Stop()
		delete(d.interactiveTimers, requestID)
	}
	d.cardMu.Unlock()
	if orig == nil {
		return
	}
	if expired, err := renderer.RenderInteractiveExpired(orig); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), noticeSendTimeout)
		defer cancel()
		_ = d.bot.UpdateCard(ctx, messageID, expired)
	}
}

// finalizeLinkedInteractive flips every still-pending interactive card tied to
// promptID to a finished state now that the result card has landed. Without
// this a permission/question card would stay grey forever after the turn it
// gatekept completed. No-op when the turn had no interactive card or the user
// already submitted (the binding is gone). Each card's TTL timer is cancelled
// so it cannot later overwrite the finalised form with an expiry notice.
//
// Lock ordering: remove TurnManager bindings first, then clean up the paired
// cardMu state. This avoids lock-order inversion with sendInteractive.
func (d *Dispatcher) finalizeLinkedInteractive(ctx context.Context, promptID string) {
	bindings := d.turns.UnbindInteractiveByPromptID(promptID)
	// Capture the original card bytes while holding cardMu, then release the
	// lock before doing synchronous Feishu PATCH calls.
	origs := make(map[string][]byte, len(bindings))
	d.cardMu.Lock()
	for _, b := range bindings {
		origs[b.RequestID] = d.cards[b.RequestID]
		if t := d.interactiveTimers[b.RequestID]; t != nil {
			t.Stop()
			delete(d.interactiveTimers, b.RequestID)
		}
		delete(d.cards, b.RequestID)
	}
	d.cardMu.Unlock()
	for _, b := range bindings {
		if orig := origs[b.RequestID]; orig != nil {
			if fin, ferr := renderer.RenderInteractiveFinalized(orig); ferr == nil {
				_ = d.bot.UpdateCard(ctx, b.MessageID, fin)
			}
		}
	}
}

// DispatchCardAction handles a Feishu interactive-card button click: flips
// the card to its submitted state, then forwards the answer to the bound
// backend as a TypeAnswer event. Idempotent per requestID (actionIDs dedup)
// so a double-click does not double-send.
//
// NOTE: Feishu's card.action.trigger has a ~3-5s "click-handling window"
// during which any PatchMessage call gets silently reverted. The picker
// path (handleBackendChoice) therefore delays its PATCH past this window
// via a background goroutine. The permission/question path here still
// PATCHes immediately — those cards are typically opened by a single user
// who rarely re-views them, so the rollback rarely bites. If it does,
// apply the same delayed-PATCH pattern used by the picker.
func (d *Dispatcher) DispatchCardAction(ctx context.Context, action *feishu.CardAction) error {
	kind := d.auditCardAction(action)

	// Frontend-owned card (the /backend picker): consume the click directly —
	// no requestID, no answer forwarding to a backend.
	//
	// The dedup check below is intentionally skipped here: backend-picker
	// clicks carry no requestID, so a malicious or rapid double-click could
	// bypass the actionIDs guard. UX-wise the picker buttons are disabled
	// client-side the instant the first click lands, so a real user cannot
	// fire two binding changes; only a constructed request bypassing the
	// disabled state could. Acceptable: /backend use is idempotent for the
	// same target (SetBackend is a store), and a malicious opener already
	// has its own backend privileges. Revisit if picker ever triggers a
	// non-idempotent side effect.
	if kind == "backend" {
		return d.handleBackendChoice(ctx, action)
	}

	requestID := requestIDFromValue(action.Value)
	if requestID != "" && !d.actionIDs.Add(requestID) {
		return nil
	}
	if requestID != "" {
		if messageID, ok := d.turns.InteractiveMessageID(requestID); ok {
			d.flipInteractiveSubmitted(ctx, action, requestID, messageID)
			// Deliberately NOT unbinding: keep requestID→messageID so the
			// turn-completing result can finalize this card in place.
		}
	}

	answer := d.buildAnswerPayload(action, requestID)
	return d.forwardCardAnswer(ctx, action.ChatID, requestID, answer)
}

// auditCardAction logs the operator and the shape of the incoming card action.
// It returns the action kind so the caller can branch on /backend vs real answers.
func (d *Dispatcher) auditCardAction(action *feishu.CardAction) string {
	// Audit the operator before routing. Card callbacks are not authenticated
	// against the original turn's sender (group-chat collaboration model), so
	// recording UserOpenID is the minimum trail for "who acted on whose card".
	// Info (not Debug): a security-relevant event must survive the default
	// log level, otherwise the audit vanishes in production.
	kind, _ := action.Value["kind"].(string)
	d.logger.Load().Info("card action",
		log.FieldChatID, action.ChatID,
		log.FieldEventType, kind,
		"operator_openid", action.UserOpenID,
		"request_id", requestIDFromValue(action.Value))
	// kind=="" 意味着 button value 没回传（schema 2.0 下顶层 value 字段
	// 已是 historical attribute）。Warn 截断后的 value（可能含表单 PII，
	// low-18），足以区分「空 map」与「回了部分字段但缺 kind」两种失效形态。
	if kind == "" {
		valueJSON, _ := json.Marshal(action.Value)
		d.logger.Load().Warn("card action: empty kind in value",
			log.FieldChatID, action.ChatID,
			log.FieldMessageID, action.MessageID,
			"value", strutil.Truncate(string(valueJSON), 300))
	}
	return kind
}

// flipInteractiveSubmitted rewrites an interactive card to its submitted state,
// schedules a delayed fallback PATCH past Feishu's click-handling window, and
// caches the submitted bytes for later finalization. The TTL timer is stopped
// but the binding is kept so finalizeLinkedInteractive can advance the same
// card once the turn completes.
func (d *Dispatcher) flipInteractiveSubmitted(ctx context.Context, action *feishu.CardAction, requestID, messageID string) {
	// Stop the TTL timer under the lock so expireInteractive cannot race this
	// flip. The cache and binding are KEPT (not deleted): the cached bytes are
	// rewritten to the submitted form below so finalizeLinkedInteractive can
	// later advance the SAME card to "finalized". Deleting here (the prior
	// behaviour) stranded submitted cards on "处理中" amber forever.
	d.cardMu.Lock()
	orig := d.cards[requestID]
	if t := d.interactiveTimers[requestID]; t != nil {
		t.Stop()
		delete(d.interactiveTimers, requestID)
	}
	d.cardMu.Unlock()
	if orig == nil {
		return
	}
	sub, err := renderer.RenderInteractiveSubmitted(orig, submitSummary(action))
	if err != nil {
		return
	}
	_ = d.bot.UpdateCard(ctx, messageID, sub)
	d.scheduleSubmitFallback(ctx, action.ChatID, requestID, messageID, sub)
	d.cacheSubmittedCard(requestID, sub)
}

// scheduleSubmitFallback re-PATCHes the submitted card after Feishu's ~3-5s
// click-handling window to guard against silent reverts. It is a no-op if the
// binding has already been finalized (and the card flipped to the terminal
// green frame) during the wait.
func (d *Dispatcher) scheduleSubmitFallback(ctx context.Context, chatID, requestID, messageID string, card []byte) {
	fbDelay := d.cardPatchDelay
	if fbDelay <= 0 {
		fbDelay = cardPatchDelayDefault
	}
	goSafe(func() {
		time.Sleep(fbDelay)
		if _, ok := d.turns.InteractiveMessageID(requestID); !ok {
			return
		}
		patchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), noticeSendTimeout)
		defer cancel()
		if err := d.bot.UpdateCardVerified(patchCtx, messageID, card); err != nil {
			if l := d.logger.Load(); l != nil {
				l.Warn("delayed submit verified update failed",
					log.FieldChatID, chatID,
					log.FieldMessageID, messageID,
					log.FieldError, err.Error())
			}
		}
	})
}

// cacheSubmittedCard stores the submitted card bytes so finalizeLinkedInteractive
// can render finalized-from-submitted and preserve the "✓ 已回答" echo (C5).
// It only writes when the binding still exists, otherwise the entry would be an
// orphan never reaped by SweepInteractive.
func (d *Dispatcher) cacheSubmittedCard(requestID string, card []byte) {
	// Query the binding before taking cardMu to preserve the TurnManager →
	// cardMu lock ordering.
	_, hasBinding := d.turns.InteractiveMessageID(requestID)
	d.cardMu.Lock()
	if hasBinding {
		d.cards[requestID] = card
	}
	d.cardMu.Unlock()
}

// buildAnswerPayload constructs the Answer event payload from the form values or
// the explicit choice value.
func (d *Dispatcher) buildAnswerPayload(action *feishu.CardAction, requestID string) *protocol.AnswerPayload {
	answer := &protocol.AnswerPayload{ChatID: action.ChatID, RequestID: requestID, MessageID: action.MessageID}
	if len(action.FormValue) > 0 {
		answer.Choices, answer.Custom = parseQuestionFormValue(action.FormValue)
	} else if c, ok := action.Value["choice"].(string); ok {
		answer.Choice = c
		answer.Choices = []string{c}
	}
	return answer
}

// forwardCardAnswer resolves the chat's backend and forwards the answer payload
// as a TypeAnswer event.
func (d *Dispatcher) forwardCardAnswer(ctx context.Context, chatID, requestID string, answer *protocol.AnswerPayload) error {
	d.logger.Load().Debug("card action: sending answer to backend",
		"chat_id", chatID,
		"request_id", requestID,
		"message_id", answer.MessageID,
		"choice", answer.Choice,
		"choices", answer.Choices,
		"custom", answer.Custom)
	ev := &protocol.Event{Type: protocol.TypeAnswer, PromptID: answer.MessageID, Answer: answer}
	if d.router == nil {
		d.logger.Load().Debug("card action: router is nil, skipping")
		return nil
	}
	backendID, err := d.router.Resolve(chatID)
	if err != nil {
		d.logger.Load().Debug("card action: failed to resolve backend",
			"chat_id", chatID,
			log.FieldError, err)
		return err
	}
	d.logger.Load().Debug("card action: sending event to backend",
		"chat_id", chatID,
		"backend_id", backendID,
		"request_id", requestID)
	if err := d.registry.SendEvent(backendID, ev); err != nil {
		d.logger.Load().Warn("card action: SendEvent failed",
			"chat_id", chatID,
			"backend_id", backendID,
			"request_id", requestID,
			log.FieldError, err)
		return err
	}
	d.logger.Load().Debug("card action: event sent successfully",
		"chat_id", chatID,
		"backend_id", backendID,
		"request_id", requestID)
	return nil
}
