package feishufront

import (
	"context"
	"time"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/feishufront/renderer"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// sendInteractive renders and sends a permission-request or question card,
// binds the requestID → messageID so a later card action can find it, caches
// the rendered card bytes for the submitted/expired/finalised state flips,
// and schedules the TTL expiry notice. Called by DispatchControl for
// TypeQuestion controls.
func (d *Dispatcher) sendInteractive(ctx context.Context, ctrl *protocol.Control, backendType string) error {
	_, _, chatID, footer := d.resolveFooter(ctrl, backendType)
	footer.Status = "待确认"
	header := cardkit.HeaderInfo{BackendType: backendType}
	card, err := renderer.RenderQuestion(ctrl, header, footer)
	if err != nil {
		return err
	}
	requestID := ctrl.Question.RequestID
	messageID, err := d.bot.SendCard(ctx, chatID, card, "")
	if err != nil {
		return err
	}
	if requestID != "" {
		// Evict expired interactive bindings (and their cached card bytes)
		// before adding the new one, so cards ignored by the user — or left
		// dangling when the backend crashes mid-answer — do not leak.
		for _, rid := range d.turns.SweepInteractive() {
			d.cardMu.Lock()
			delete(d.cards, rid)
			d.cardMu.Unlock()
		}
		d.turns.BindInteractive(requestID, messageID, ctrl.PromptID)
		d.cardMu.Lock()
		d.cards[requestID] = card
		// Schedule the expiry notice; if the user never responds within the
		// TTL the card is flipped to a "已失效" state instead of waiting grey
		// forever. Stopped on submit (DispatchCardAction).
		reqID := requestID
		msgID := messageID
		d.interactiveTimers[requestID] = time.AfterFunc(cardkit.InteractiveTimeout, func() {
			d.expireInteractive(reqID, msgID)
		})
		d.cardMu.Unlock()
	}
	return nil
}

// submitSummary renders the "✓ 你选择了「允许」" / "✓ 已提交" line that
// RenderInteractiveSubmitted prepends to a submitted card. A permission card
// carries the choice in value["choice"]; a question card's selections are
// free-form and kept short — confirming submission is enough. The generic
// question line ("正在处理") fits both model/agent pickers and any future
// question card: the per-turn result, when it lands, tells the user what
// actually happened.
func submitSummary(action *feishu.CardAction) string {
	if c, ok := action.Value["choice"].(string); ok && c != "" {
		return "✓ 你选择了「" + choiceLabel(c) + "」"
	}
	return "✓ 已提交，正在处理…"
}

// choiceLabel turns the machine choice value into the button label the user
// actually clicked, so the confirmation echo matches what was on screen.
func choiceLabel(c string) string {
	switch c {
	case "allow":
		return "允许"
	case "deny":
		return "拒绝"
	}
	return c
}

// expireInteractive flips a still-pending interactive card to its expired
// state. Called by the TTL timer. If the user submitted in the meantime the
// binding is already gone (InteractiveMessageID returns false) and this is a
// no-op. cardMu serialises against a concurrent submit so the worst case is a
// benign overwrite, not a data race.
func (d *Dispatcher) expireInteractive(requestID, messageID string) {
	d.cardMu.Lock()
	orig := d.cards[requestID]
	delete(d.cards, requestID)
	delete(d.interactiveTimers, requestID)
	d.cardMu.Unlock()
	if orig == nil {
		return
	}
	if expired, err := renderer.RenderInteractiveExpired(orig); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), noticeSendTimeout)
		defer cancel()
		_ = d.bot.UpdateCard(ctx, messageID, expired)
	}
	d.turns.UnbindInteractive(requestID)
}

// finalizeLinkedInteractive flips every still-pending interactive card tied to
// promptID to a finished state now that the result card has landed. Without
// this a permission/question card would stay grey forever after the turn it
// gatekept completed. No-op when the turn had no interactive card or the user
// already submitted (the binding is gone). Each card's TTL timer is cancelled
// so it cannot later overwrite the finalised form with an expiry notice.
func (d *Dispatcher) finalizeLinkedInteractive(ctx context.Context, promptID string) {
	for _, pair := range d.turns.InteractiveByPromptID(promptID) {
		requestID, messageID := pair[0], pair[1]
		d.cardMu.Lock()
		orig := d.cards[requestID]
		if t := d.interactiveTimers[requestID]; t != nil {
			t.Stop()
			delete(d.interactiveTimers, requestID)
		}
		delete(d.cards, requestID)
		d.cardMu.Unlock()
		d.turns.UnbindInteractive(requestID)
		if orig == nil {
			continue
		}
		if fin, ferr := renderer.RenderInteractiveFinalized(orig); ferr == nil {
			_ = d.bot.UpdateCard(ctx, messageID, fin)
		}
	}
}

// DispatchCardAction handles a Feishu interactive-card button click: flips the
// card to its submitted state, then forwards the answer to the bound backend
// as a TypeAnswer event. Idempotent per requestID (actionIDs dedup) so a
// double-click does not double-send.
func (d *Dispatcher) DispatchCardAction(ctx context.Context, action *feishu.CardAction) error {
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
	// Frontend-owned card (the /backend picker): consume the click directly —
	// no requestID, no answer forwarding to a backend.
	if kind == "backend" {
		return d.handleBackendChoice(ctx, action)
	}
	requestID := requestIDFromValue(action.Value)
	if requestID != "" && !d.actionIDs.Add(requestID) {
		return nil
	}
	if requestID != "" {
		if messageID, ok := d.turns.InteractiveMessageID(requestID); ok {
			d.cardMu.Lock()
			orig := d.cards[requestID]
			if t := d.interactiveTimers[requestID]; t != nil {
				t.Stop()
				delete(d.interactiveTimers, requestID)
			}
			d.cardMu.Unlock()
			if orig != nil {
				if sub, err := renderer.RenderInteractiveSubmitted(orig, submitSummary(action)); err == nil {
					_ = d.bot.UpdateCard(ctx, messageID, sub)
				}
			}
			// The card has been submitted; the entry is no longer needed.
			d.cardMu.Lock()
			delete(d.cards, requestID)
			d.cardMu.Unlock()
			d.turns.UnbindInteractive(requestID)
		}
	}
	answer := &protocol.AnswerPayload{ChatID: action.ChatID, RequestID: requestID}
	if len(action.FormValue) > 0 {
		answer.Choices, answer.Custom = parseQuestionFormValue(action.FormValue)
	} else if c, ok := action.Value["choice"].(string); ok {
		answer.Choice = c
	}
	ev := &protocol.Event{Type: protocol.TypeAnswer, PromptID: action.MessageID, Answer: answer}
	if d.router == nil {
		return nil
	}
	backendID, err := d.router.Resolve(action.ChatID)
	if err != nil {
		return err
	}
	return d.registry.SendEvent(backendID, ev)
}
