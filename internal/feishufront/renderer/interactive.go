package renderer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/justphantom/lark-bridge/internal/feishufront/cardkit"
	"github.com/justphantom/lark-bridge/internal/protocol"
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

// maxQuestions bounds questions per card. Each question yields 2-3 form
// elements (label markdown + select + optional custom input / 省略提示),
// plus the trailing wait-notice, submit button and footer. 15 questions
// keeps the worst case (15×3 + 3 = 48) just under cardkit.MaxCardElements,
// so a backend emitting 25+ questions no longer triggers a server-side
// 230025 rejection that the user sees as "卡渲染失败".
const maxQuestions = 15

// maxPermissionOptions bounds options per permission card. Each option is
// one button action; reserve 3 slots for the message body, wait notice,
// and footer so the card stays under cardkit.MaxCardElements.
const maxPermissionOptions = cardkit.MaxCardElements - 3

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
	shown := len(q.Questions)
	if shown > maxQuestions {
		shown = maxQuestions
	}
	for idx := range shown {
		item := q.Questions[idx]
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
	if len(q.Questions) > maxQuestions {
		formElems = append(formElems, cardkit.MarkdownElement(fmt.Sprintf("…共 %d 个问题，已省略 %d 项", len(q.Questions), len(q.Questions)-maxQuestions)))
	}
	formElems = append(formElems, cardkit.MarkdownElement(fmt.Sprintf("⏳ 等待你的确认（%d 分钟后自动失效）", int(cardkit.InteractiveTimeout.Minutes()))))
	submit := cardkit.SubmitButtonAction("提交",
		map[string]any{"requestID": q.RequestID, "kind": "question"}, true)
	formElems = append(formElems, cardkit.Element(submit))
	form := cardkit.FormElement("question_form", formElems)
	return cardkit.Card(header, footer, []cardkit.Element{form}, nil)
}

// RenderPermission builds a permission card: the message as markdown, then one
// button per option placed in the card's actions (not a form), so a click
// submits immediately without a separate "提交" step. Each button carries
// kind="permission" + the option's Value as "choice" so DispatchCardAction
// routes the click and the consumer reads Choices[0].
//
// Structured body: when Title is set, the renderer uses Type/Title/Detail
// (badge + headline + code-block detail) and ignores Message; otherwise it
// falls back to Message so a caller that sets only Message (e.g. a mode
// picker) renders exactly as before.
func RenderPermission(ctrl *protocol.Control, header cardkit.HeaderInfo, footer cardkit.FooterInfo) ([]byte, error) {
	header.Template = "orange"
	if header.Title == "" {
		header.Title = "权限请求"
	}
	p := ctrl.Permission
	var elements []cardkit.Element
	if body := permissionStructuredBody(p); body != "" {
		elements = append(elements, cardkit.MarkdownElement(body))
	} else if msg := truncateRunes(p.Message, maxInteractiveBodyRunes); msg != "" {
		elements = append(elements, cardkit.MarkdownElement(msg))
	}
	elements = append(elements, cardkit.MarkdownElement(fmt.Sprintf("⏳ 等待你的确认（%d 分钟后自动失效）", int(cardkit.InteractiveTimeout.Minutes()))))
	opts := p.Options
	if len(opts) > maxPermissionOptions {
		opts = opts[:maxPermissionOptions]
	}
	actions := make([]cardkit.Action, 0, len(opts))
	for _, opt := range opts {
		actions = append(actions, cardkit.ButtonAction(
			truncateRunes(opt.Label, maxInteractiveBodyRunes), "permission",
			map[string]any{"requestID": p.RequestID, "choice": opt.Value}, false, false))
	}
	return cardkit.Card(header, footer, elements, actions)
}

// permissionStructuredBody renders the typed permission body: a bold Type
// badge, the Title headline, and Detail in a code block. Returns "" when Title
// is empty, signalling the caller to fall back to the flat Message. cardkit
// has no collapsible element, so Detail is a code block (always visible) rather
// than a fold — a future cardkit fold element would change only this helper.
// Detail equal to Title is dropped to avoid a duplicate block.
func permissionStructuredBody(p *protocol.PermissionPayload) string {
	if p.Title == "" {
		return ""
	}
	var b strings.Builder
	if ty := truncateRunes(p.Type, 32); ty != "" {
		b.WriteString("**" + ty + "**\n\n")
	}
	b.WriteString(truncateRunes(p.Title, maxInteractiveBodyRunes))
	if d := truncateRunes(p.Detail, maxInteractiveBodyRunes); d != "" && d != p.Title {
		b.WriteString("\n```\n" + d + "\n```")
	}
	return b.String()
}

// RenderInteractive dispatches to the per-type interactive renderer: a
// permission card renders as buttons; a single-question, single-select
// question with ≤4 options and no custom input also renders as immediate-click
// buttons (no dropdown+submit ceremony); every other interactive control
// renders as the question dropdown form.
func RenderInteractive(ctrl *protocol.Control, header cardkit.HeaderInfo, footer cardkit.FooterInfo) ([]byte, error) {
	if ctrl.Type == protocol.TypePermission {
		return RenderPermission(ctrl, header, footer)
	}
	if canRenderQuestionAsButtons(ctrl.Question) {
		return RenderQuestionButtons(ctrl, header, footer)
	}
	return RenderQuestion(ctrl, header, footer)
}

// canRenderQuestionAsButtons reports whether a question is small enough to
// drop the dropdown+submit form in favour of immediate-click buttons: exactly
// one question, single-select, 1-4 options, no custom input. Multi-question,
// multi-select, custom-input, or many-option questions still need the form.
func canRenderQuestionAsButtons(q *protocol.QuestionPayload) bool {
	if q == nil || len(q.Questions) != 1 {
		return false
	}
	item := q.Questions[0]
	return !item.Multiple && !item.Custom && len(item.Options) >= 1 && len(item.Options) <= 4
}

// RenderQuestionButtons renders a single-question, single-select question with
// ≤4 options and no custom input as immediate-click buttons, mirroring the
// permission card. Each button carries kind="question" + the option label as
// "choice"; DispatchCardAction sets Choices=[choice], which
// questionReplyFromAnswer maps to answers[0] exactly as the dropdown path does
// (SelectOption uses label-as-value too), so the bridge mapping is unchanged.
func RenderQuestionButtons(ctrl *protocol.Control, header cardkit.HeaderInfo, footer cardkit.FooterInfo) ([]byte, error) {
	header.Template = "orange"
	if header.Title == "" {
		header.Title = "提问"
	}
	q := ctrl.Question
	item := q.Questions[0]
	var elements []cardkit.Element
	if label := truncateRunes(item.Label, maxInteractiveBodyRunes); label != "" {
		elements = append(elements, cardkit.MarkdownElement("**"+label+"**"))
	}
	elements = append(elements, cardkit.MarkdownElement(fmt.Sprintf("⏳ 等待你的确认（%d 分钟后自动失效）", int(cardkit.InteractiveTimeout.Minutes()))))
	actions := make([]cardkit.Action, 0, len(item.Options))
	for _, opt := range item.Options {
		actions = append(actions, cardkit.ButtonAction(
			truncateRunes(opt, maxInteractiveBodyRunes), "question",
			map[string]any{"requestID": q.RequestID, "choice": opt}, false, false))
	}
	return cardkit.Card(header, footer, elements, actions)
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
