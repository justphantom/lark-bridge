package opencodeservebridge

import (
	"context"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// interactiveOpTimeout bounds the question-card emit and the serve reply
// call. Both run on a fresh ctx: on abort the prompt ctx is already
// cancelled, but the reject must still reach the serve server or its agent
// hangs forever waiting for an answer.
const interactiveOpTimeout = 10 * time.Second

// permissionOption maps a permission card's option label to the serve reply
// value. Anything unmapped falls back to reject (safe default). The "始终允许"
// label carries a scope hint (全局) so the user understands before clicking
// that PermissionReplyAlways is a persistent global grant, not a per-turn one.
var permissionOptions = []struct {
	label string
	reply string
}{
	{"允许一次", oc.PermissionReplyOnce},
	{"始终允许（全局）", oc.PermissionReplyAlways},
	{"拒绝", oc.PermissionReplyReject},
}

// handlePermissionAsked runs the full interaction for one permission.asked
// event: emit a gate banner on the progress card, emit the standalone
// permission card, wait for the user, reply to the serve server, then flip
// the banner to answered/denied. Runs in its own goroutine spawned by
// streamRun; ctx is the prompt ctx so an abort/timeout turns into a reject
// instead of a hung serve-side agent.
func (h *Handler) handlePermissionAsked(ctx context.Context, chatID, promptID string, p *oc.PermissionAskedData) {
	h.Logger.Debug("permission asked",
		log.FieldChatID, chatID,
		"prompt_id", promptID,
		"request_id", p.ID,
		"permission", p.Permission)
	// Banner on the streaming progress card: the agent is blocked until the
	// user acts. The standalone permission card carries the buttons; this
	// banner makes the blockage visible where the user is already watching.
	h.emitAsync(promptID, gateControl(chatID, promptID, "permission", "waiting", gatePermissionSummary(p)))
	opts := make([]protocol.PermissionOption, len(permissionOptions))
	for i, o := range permissionOptions {
		opts[i] = protocol.PermissionOption{Label: o.label, Value: o.label}
	}
	choice, _, err := bridgebase.AskPermission(ctx, h.Answers, h.Emit, chatID, promptID, p.ID, "权限", permissionMessage(p), opts, false)
	reply := oc.PermissionReplyReject
	if err == nil {
		reply = permissionReplyOf(choice)
	}
	directory := h.directoryOf(chatID)
	rctx, cancel := context.WithTimeout(context.Background(), interactiveOpTimeout)
	defer cancel()
	if err := h.agent.ReplyPermission(rctx, p.ID, directory, reply, ""); err != nil {
		h.Logger.Warn("reply permission failed",
			log.FieldChatID, chatID,
			"request_id", p.ID,
			"directory", directory,
			log.FieldError, err)
		return
	}
	h.Logger.Debug("permission replied",
		log.FieldChatID, chatID,
		"request_id", p.ID,
		"directory", directory,
		"reply", reply)
	// Flip the progress banner to the terminal state. The standalone card's
	// own submit/finalize lifecycle is unaffected; this only updates the
	// one-line banner on the streaming card. (The prior TypeText echo here
	// was dead — the dispatcher drops TypeText — so it never reached the card.)
	state := "answered"
	summary := choice
	if reply == oc.PermissionReplyReject {
		state = "denied"
		summary = ""
	}
	h.emitAsync(promptID, gateControl(chatID, promptID, "permission", state, summary))
}

// permissionReplyOf maps a card option label to a serve reply value.
// Unknown/empty labels reject: silently granting a permission the user did
// not explicitly pick is worse than a spurious denial.
func permissionReplyOf(label string) string {
	for _, o := range permissionOptions {
		if o.label == label {
			return o.reply
		}
	}
	return oc.PermissionReplyReject
}

// handleQuestionAsked mirrors handlePermissionAsked for question.asked. An
// incomplete answer (user cancelled, timed out, or skipped a question)
// rejects the request so the serve-side agent is released. The progress-card
// banner tracks waiting→answered/denied just like the permission path.
func (h *Handler) handleQuestionAsked(ctx context.Context, chatID, promptID string, q *oc.QuestionAskedData) {
	h.Logger.Debug("question asked",
		log.FieldChatID, chatID,
		"prompt_id", promptID,
		"request_id", q.ID,
		"question_count", len(q.Questions))
	h.emitAsync(promptID, gateControl(chatID, promptID, "question", "waiting", gateQuestionSummary(q)))
	items := make([]protocol.QuestionItem, 0, len(q.Questions))
	for _, qi := range q.Questions {
		item := protocol.QuestionItem{Label: qi.Question, Multiple: qi.Multiple, Custom: qi.Custom}
		for _, o := range qi.Options {
			item.Options = append(item.Options, o.Label)
		}
		items = append(items, item)
	}
	ans := h.askAndWait(ctx, chatID, promptID, &protocol.QuestionPayload{
		RequestID: q.ID,
		PromptID:  promptID,
		Questions: items,
	})
	h.Logger.Debug("question answer received",
		log.FieldChatID, chatID,
		"request_id", q.ID,
		"has_answer", ans != nil)
	if ans != nil {
		h.Logger.Debug("question answer details",
			log.FieldChatID, chatID,
			"request_id", q.ID,
			"choice", ans.Choice,
			"choices", ans.Choices,
			"custom", ans.Custom)
	}
	rctx, cancel := context.WithTimeout(context.Background(), interactiveOpTimeout)
	defer cancel()
	directory := h.directoryOf(chatID)
	// Multi-question + custom-input rejection: opencode's question card lets
	// the user type a free-form value alongside the option dropdowns, but a
	// single Custom string cannot map to multiple questions unambiguously.
	// Mapping to answers[0] silently drops the rest; rejecting with an
	// explicit notice tells the user to pick options per question instead
	// of re-submitting a custom value the bridge will keep refusing. The
	// rare-batch path (opencode's question tool almost always asks one at a
	// time) is preserved: single-question + Custom still works as before.
	if ans != nil && ans.Custom != "" && len(q.Questions) > 1 {
		h.emitAsync(promptID, &protocol.Control{
			Type:   protocol.TypeNotice,
			ChatID: chatID,
			Notice: &protocol.NoticePayload{
				Level:           "warning",
				Title:           "不支持批量自定义",
				Message:         "本次提问包含多个问题，自定义输入仅支持单个问题场景。请逐个选择 option 后重新提交。",
				UpdateMessageID: ans.MessageID,
			},
		})
		if err := h.agent.RejectQuestion(rctx, q.ID, directory); err != nil {
			h.Logger.Warn("reject question (multi+custom) failed",
				log.FieldChatID, chatID,
				log.FieldSessionID, q.ID,
				log.FieldError, err)
		}
		// Banner reflects the reject so the progress card does not linger
		// in the ⏸ waiting state after the standalone card already warned.
		h.emitAsync(promptID, gateControl(chatID, promptID, "question", "denied", ""))
		return
	}
	reply, ok := questionReplyFromAnswer(q, ans)
	var err error
	if !ok {
		h.Logger.Debug("rejecting question",
			log.FieldChatID, chatID,
			"request_id", q.ID,
			"directory", directory)
		err = h.agent.RejectQuestion(rctx, q.ID, directory)
	} else {
		h.Logger.Debug("replying question",
			log.FieldChatID, chatID,
			"request_id", q.ID,
			"directory", directory,
			"answer_count", len(reply.Answers))
		err = h.agent.ReplyQuestion(rctx, q.ID, directory, reply)
	}
	if err != nil {
		h.Logger.Warn("reply question failed",
			log.FieldChatID, chatID,
			"request_id", q.ID,
			"directory", directory,
			log.FieldError, err)
		return
	}
	// Flip the progress banner to the terminal state. (The prior TypeText
	// echo was dead — the dispatcher drops TypeText — so it never rendered.)
	state := "answered"
	summary := bridgebase.PickAnswerValue(ans)
	if !ok {
		state = "denied"
		summary = ""
	}
	h.emitAsync(promptID, gateControl(chatID, promptID, "question", state, summary))
}
