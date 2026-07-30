package renderer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
)

// This file holds the interactive-card state flips: functions that take an
// already-rendered card and mutate it into its submitted / expired / finalized
// form. They keep the initial-render builders (RenderPermission /
// RenderQuestion / RenderQuestionButtons in interactive.go) focused on first
// paint, and centralise the cross-cutting card-mutation helpers
// (prependMarkdown / rewriteFooterStatus / disableButtons) the flips share.

// RenderInteractiveSubmitted takes an already-rendered interactive card and
// flips every button to disabled, the primary one labelled "已提交" and the
// rest "处理中" (R4). summary is the user's choice (e.g. "✓ 你选择了「允许」")
// prepended to the body so the card confirms what was picked instead of going
// silently grey. The footer status word is flipped from "待确认" to "处理中"
// so the card reads as advancing past the pending state. Buttons may sit
// inside an action container (permission/picker) or nested inside a form
// container (question), so the walk recurses into every "elements" list.
func RenderInteractiveSubmitted(originalCard []byte, summary string) ([]byte, error) {
	var card map[string]any
	if err := json.Unmarshal(originalCard, &card); err != nil {
		return nil, err
	}
	if summary != "" {
		prependMarkdown(card, summary)
	}
	rewriteFooterStatus(card, "处理中")
	disableButtons(card)
	return json.Marshal(card)
}

// RenderInteractiveExpired flips a pending interactive card to its expired
// form: buttons disabled and a "已超过 InteractiveTimeout 未响应" line prepended
// so a user returning to a stale card understands why the backend stopped waiting.
func RenderInteractiveExpired(originalCard []byte) ([]byte, error) {
	return finalizeInteractiveCard(originalCard, fmt.Sprintf("⊘ 此请求已超过 %d 分钟未响应，已自动失效。", int(cardkit.InteractiveTimeout.Minutes())), "已失效")
}

// RenderInteractiveFinalized flips a submitted interactive card to its
// finished form once the turn's result card has been delivered, so the card
// does not linger grey forever. When the card already shows a submit
// confirmation ("✓ …", written by RenderInteractiveSubmitted) that line is
// kept — it tells the user what they picked — and only the footer status is
// flipped. When no confirmation is present (the turn ended without the user
// acting on this card), the generic "结果见上方" pointer is prepended.
func RenderInteractiveFinalized(originalCard []byte) ([]byte, error) {
	var card map[string]any
	if err := json.Unmarshal(originalCard, &card); err != nil {
		return nil, err
	}
	if !firstMarkdownStartsWith(card, "✓") {
		prependMarkdown(card, "✓ 本轮已完成，结果见上方卡片。")
	}
	rewriteFooterStatus(card, "已完成")
	disableButtons(card)
	return json.Marshal(card)
}

// firstMarkdownStartsWith reports whether the first (top-most) markdown
// element in the card starts with prefix. Only the first markdown is
// consulted so a stray "✓" deeper in the card cannot mask a missing top
// confirmation. Returns false when no markdown element exists.
func firstMarkdownStartsWith(card map[string]any, prefix string) bool {
	elements, _ := card["elements"].([]any)
	for _, el := range elements {
		em, ok := el.(map[string]any)
		if !ok {
			continue
		}
		if em["tag"] == "markdown" {
			c, _ := em["content"].(string)
			return strings.HasPrefix(c, prefix)
		}
	}
	return false
}

// finalizeInteractiveCard is the shared tail for the expired/finalised forms:
// prepend a one-line status notice, rewrite the footer status word, and
// disable every button.
func finalizeInteractiveCard(originalCard []byte, notice, footerStatus string) ([]byte, error) {
	var card map[string]any
	if err := json.Unmarshal(originalCard, &card); err != nil {
		return nil, err
	}
	prependMarkdown(card, notice)
	rewriteFooterStatus(card, footerStatus)
	disableButtons(card)
	return json.Marshal(card)
}

// prependMarkdown inserts a markdown element at the top of the card elements
// so a status line (choice echo / expiry notice) reads above the original content.
func prependMarkdown(card map[string]any, text string) {
	elements, _ := card["elements"].([]any)
	ahead := []any{map[string]any{"tag": "markdown", "content": text}}
	card["elements"] = append(ahead, elements...)
}

// rewriteFooterStatus replaces the leading status word in the card's footer
// line. The footer is the last element (a div whose text content is
// "<status> · backendType · …"). Only a footer still showing a NON-TERMINAL
// status (待确认 = pending, 处理中 = submitted/processing) is rewritten, so a
// footer already advanced to a terminal status (已完成/已失效/…) cannot be
// regressed by a later flip. Both non-terminals must be covered now that a
// submitted card's footer reads "处理中 · …" and finalize advances it to
// "已完成".
func rewriteFooterStatus(card map[string]any, newStatus string) {
	elements, _ := card["elements"].([]any)
	if len(elements) == 0 {
		return
	}
	last, _ := elements[len(elements)-1].(map[string]any)
	if last == nil || last["tag"] != "div" {
		return
	}
	text, _ := last["text"].(map[string]any)
	if text == nil {
		return
	}
	content, _ := text["content"].(string)
	for _, from := range []string{"待确认", "处理中"} {
		if strings.HasPrefix(content, from+" · ") {
			text["content"] = newStatus + content[len(from):]
			return
		}
	}
}

// disableButtons recursively walks node["elements"] AND node["actions"],
// disabling every button it finds. Schema 1.0 cards place buttons in two
// layouts: loose inside a form container's "elements" (dropdown+submit
// question cards), or grouped under an action container's "actions"
// (permission / picker / immediate-click question cards). Walking only
// "elements" left the action-container cards' option buttons clickable after
// submit — defeating the "翻灰禁用" guarantee — so both keys are traversed.
// The card root itself carries elements (schema 1.0 layout).
func disableButtons(node map[string]any) {
	for _, key := range []string{"elements", "actions"} {
		children, _ := node[key].([]any)
		for _, el := range children {
			elem, ok := el.(map[string]any)
			if !ok {
				continue
			}
			if tag, _ := elem["tag"].(string); tag == "button" {
				elem["disabled"] = true
				if text, _ := elem["text"].(map[string]any); text != nil {
					if t, _ := elem["type"].(string); t == "primary" {
						text["content"] = "已提交"
					} else {
						text["content"] = "处理中"
					}
				}
				continue
			}
			// Recurse into containers (form/column/action/...) that hold
			// their own elements or actions.
			disableButtons(elem)
		}
	}
}
