package renderer

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hu/lark-bridge/internal/feishufront/cardkit"
	"github.com/hu/lark-bridge/internal/protocol"
)

// maxInteractiveBodyRunes caps permission message and question label text.
// References cardkit.MaxBodyRunes so the interactive/notice body budget has a
// single source of truth.
const maxInteractiveBodyRunes = cardkit.MaxBodyRunes

// maxQuestionOptionRunes is the rune budget for a question card's select
// options (the concatenated option text + per-entry envelope). It is larger
// than maxInteractiveBodyRunes because model/agent/settings pickers can list
// tens of entries whose total volume is legitimate content, not prose to
// trim. 15000 accommodates ~270 average-length model names while staying well
// under Feishu's card content ceiling.
const maxQuestionOptionRunes = 15000

// interactiveTimeoutHint returns the pending-hint line appended to every
// permission/question card so the user knows a silent card will not wait
// forever. The duration comes from cardkit.InteractiveTimeout so the UI stays
// in sync with the turn manager's eviction policy.
func interactiveTimeoutHint() string {
	return fmt.Sprintf("\n\n⏳ 等待你的确认（%d 分钟后自动失效）", int(cardkit.InteractiveTimeout.Minutes()))
}

// RenderQuestion builds a question card: one block per question (label +
// options as a select/multi-select), an optional custom-input box, and a
// single submit button. All controls live inside a form container so the
// submit button (action_type=form_submit) collects their values into
// action.form_value. Component names follow the q_<idx> / custom_<idx>
// convention (Feishu requires a letter-prefixed, ≤20-char, card-unique
// name; the human Label is often Chinese and would be rejected).
func RenderQuestion(ctrl *protocol.Control, header cardkit.HeaderInfo, footer cardkit.FooterInfo) ([]byte, error) {
	header.Template = "orange"
	if header.Title == "" {
		header.Title = "提问"
	}
	q := ctrl.Question
	var formElems []cardkit.Element
	for idx, item := range q.Questions {
		formElems = append(formElems, cardkit.MarkdownElement("**"+truncateRunes(item.Label, maxInteractiveBodyRunes)+"**"))
		opts, omitted := capOptions(item.Options, maxQuestionOptionRunes)
		formElems = append(formElems, cardkit.SelectStaticElement(
			fmt.Sprintf("q_%d", idx), "请选择", opts, item.Multiple))
		if omitted > 0 {
			formElems = append(formElems, cardkit.MarkdownElement("…共 "+strconv.Itoa(omitted)+" 项已省略"))
		}
		if item.Custom {
			formElems = append(formElems, cardkit.InputElement(
				fmt.Sprintf("custom_%d", idx), "自定义输入"))
		}
	}
	formElems = append(formElems, cardkit.MarkdownElement(fmt.Sprintf("⏳ 等待你的确认（%d 分钟后自动失效）", int(cardkit.InteractiveTimeout.Minutes()))))
	submit := cardkit.SubmitButtonAction("提交",
		map[string]any{"requestID": q.RequestID, "kind": "question"}, true)
	formElems = append(formElems, cardkit.Element(submit))
	form := cardkit.FormElement("question_form", formElems)
	return cardkit.Card(header, footer, []cardkit.Element{form}, nil)
}

// RenderInteractiveSubmitted takes an already-rendered interactive card and
// flips every button to disabled, the primary one labelled "已提交" and the
// rest "处理中" (R4). summary is the user's choice (e.g. "✓ 你选择了「允许」")
// prepended to the body so the card confirms what was picked instead of going
// silently grey. The footer status word is flipped from "待确认" to "处理中"
// so the card reads as advancing past the pending state. Buttons may sit
// directly in body.elements (permission) or nested inside a form container
// (question), so the walk recurses into every "elements" list.
func RenderInteractiveSubmitted(originalCard []byte, summary string) ([]byte, error) {
	var card map[string]any
	if err := json.Unmarshal(originalCard, &card); err != nil {
		return nil, err
	}
	if summary != "" {
		prependMarkdown(card, summary)
	}
	rewriteFooterStatus(card, "处理中")
	if body, _ := card["body"].(map[string]any); body != nil {
		disableButtons(body)
	}
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
// does not linger grey forever. The notice points the user at the result.
func RenderInteractiveFinalized(originalCard []byte) ([]byte, error) {
	return finalizeInteractiveCard(originalCard, "✓ 本轮已完成，结果见上方卡片。", "已完成")
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
	if body, _ := card["body"].(map[string]any); body != nil {
		disableButtons(body)
	}
	return json.Marshal(card)
}

// prependMarkdown inserts a markdown element at the top of the card body so a
// status line (choice echo / expiry notice) reads above the original content.
func prependMarkdown(card map[string]any, text string) {
	body, _ := card["body"].(map[string]any)
	if body == nil {
		return
	}
	elements, _ := body["elements"].([]any)
	ahead := []any{map[string]any{"tag": "markdown", "content": text}}
	body["elements"] = append(ahead, elements...)
}

// rewriteFooterStatus replaces the leading status word in the card's footer
// line. The footer is the last body element (a div whose text content is
// "<status> · backendType · …"). Only a footer still showing "待确认" (the
// pending state every interactive card ships in) is rewritten, so a footer
// without a status prefix — or one already advanced — is left untouched.
func rewriteFooterStatus(card map[string]any, newStatus string) {
	body, _ := card["body"].(map[string]any)
	if body == nil {
		return
	}
	elements, _ := body["elements"].([]any)
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
	if !strings.HasPrefix(content, "待确认 · ") {
		return
	}
	text["content"] = newStatus + content[len("待确认"):]
}

// disableButtons recursively walks node["elements"] (and the card root →
// body) disabling every button it finds.
func disableButtons(node map[string]any) {
	elements, _ := node["elements"].([]any)
	for _, el := range elements {
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
		// Recurse into containers (form/column/...) that hold their own elements.
		disableButtons(elem)
	}
}

// capOptions builds the options list for a question, stopping once the
// accumulated option text exceeds maxRunes so a question with hundreds of
// options does not blow the card content limit. Returns the kept options and
// how many were dropped.
func capOptions(options []string, maxRunes int) ([]map[string]any, int) {
	kept := make([]map[string]any, 0, len(options))
	used := 0
	for _, o := range options {
		// Each option serializes to ~{"text":{"tag":"plain_text","content":"…"},"value":"…"},
		// so count the raw option text plus a fixed envelope overhead per entry.
		used += len([]rune(o)) + 40
		if used > maxRunes {
			return kept, len(options) - len(kept)
		}
		kept = append(kept, cardkit.SelectOption(o, o))
	}
	return kept, 0
}
