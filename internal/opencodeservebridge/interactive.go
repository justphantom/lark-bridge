package opencodeservebridge

import (
	"context"
	"strings"
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
// value. Anything unmapped falls back to reject (safe default).
var permissionOptions = []struct {
	label string
	reply string
}{
	{"允许一次", oc.PermissionReplyOnce},
	{"始终允许", oc.PermissionReplyAlways},
	{"拒绝", oc.PermissionReplyReject},
}

// handlePermissionAsked runs the full interaction for one permission.asked
// event: emit a question card, wait for the user, reply to the serve server.
// Runs in its own goroutine spawned by streamRun; ctx is the prompt ctx so
// an abort/timeout turns into a reject instead of a hung serve-side agent.
func (h *Handler) handlePermissionAsked(ctx context.Context, chatID, promptID string, p *oc.PermissionAskedData) {
	h.Logger.Debug("permission asked",
		log.FieldChatID, chatID,
		"prompt_id", promptID,
		"request_id", p.ID,
		"permission", p.Permission)
	label := "opencode 请求权限：" + p.Permission
	if len(p.Patterns) > 0 {
		label += "\n" + strings.Join(p.Patterns, "\n")
	}
	opts := make([]protocol.PermissionOption, len(permissionOptions))
	for i, o := range permissionOptions {
		opts[i] = protocol.PermissionOption{Label: o.label, Value: o.label}
	}
	choice, _, err := bridgebase.AskPermission(ctx, h.Answers, h.Emit, chatID, promptID, p.ID, "权限", label, opts, false)
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
	// Echo the answer onto the progress card so the user can see what was
	// answered without scrolling back to the standalone permission card.
	if err == nil && choice != "" {
		h.emitAsync(promptID, &protocol.Control{
			Type:   protocol.TypeText,
			ChatID: chatID,
			Text:   &protocol.TextPayload{Delta: "✓ 已应答权限请求: " + choice + "\n"},
		})
	}
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
// rejects the request so the serve-side agent is released.
func (h *Handler) handleQuestionAsked(ctx context.Context, chatID, promptID string, q *oc.QuestionAskedData) {
	h.Logger.Debug("question asked",
		log.FieldChatID, chatID,
		"prompt_id", promptID,
		"request_id", q.ID,
		"question_count", len(q.Questions))
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
	// Echo the answer onto the progress card so the user can see what was
	// answered without scrolling back to the standalone question card.
	if ok {
		if summary := bridgebase.PickAnswerValue(ans); summary != "" {
			h.emitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeText,
				ChatID: chatID,
				Text:   &protocol.TextPayload{Delta: "✓ 已回答: " + summary + "\n"},
			})
		}
	}
}

// directoryOf resolves the working directory bound to chatID. opencode serve
// isolates pending permission/question requests by directory, so the reply
// must carry the same directory the Run used or serve returns 404. Returns
// "" when no binding exists (the reply then hits serve's default workspace,
// which is correct only for a default-directory session).
func (h *Handler) directoryOf(chatID string) string {
	if b, ok := h.Router.Lookup(chatID); ok {
		return b.Directory
	}
	return ""
}

// questionReplyFromAnswer builds the serve reply from the card answer.
// Multi-select values arrive comma-joined per question (frontend form
// convention); custom input answers the first question (opencode's question
// tool rarely batches, and per-question custom mapping is ambiguous).
// ok=false means the answer is incomplete and the request should be rejected.
func questionReplyFromAnswer(q *oc.QuestionAskedData, ans *protocol.AnswerPayload) (*oc.QuestionReply, bool) {
	if ans == nil {
		return nil, false
	}
	answers := make([][]string, len(q.Questions))
	for i := range q.Questions {
		if i < len(ans.Choices) && ans.Choices[i] != "" {
			answers[i] = strings.Split(ans.Choices[i], ",")
		}
	}
	if ans.Custom != "" && len(answers) > 0 && len(answers[0]) == 0 {
		answers[0] = []string{ans.Custom}
	}
	for _, a := range answers {
		if len(a) == 0 {
			return nil, false
		}
	}
	return &oc.QuestionReply{Answers: answers}, true
}

// askAndWait registers the request with the answer broker, emits the
// question card, and blocks for the user's answer. nil means no answer
// (prompt cancelled, wait timeout, duplicate request, or emit failure);
// callers translate nil into a reject.
func (h *Handler) askAndWait(ctx context.Context, chatID, promptID string, q *protocol.QuestionPayload) *protocol.AnswerPayload {
	h.Logger.Debug("askAndWait: registering request",
		log.FieldChatID, chatID,
		"prompt_id", promptID,
		"request_id", q.RequestID)
	ch, ok := h.Answers.Register(q.RequestID)
	if !ok {
		h.Logger.Warn("duplicate interactive request",
			log.FieldChatID, chatID,
			"request_id", q.RequestID)
		return nil
	}
	h.Logger.Debug("askAndWait: request registered, emitting card",
		log.FieldChatID, chatID,
		"prompt_id", promptID,
		"request_id", q.RequestID)
	ectx, ecancel := context.WithTimeout(h.AppCtx, interactiveOpTimeout)
	err := h.emit(ectx, promptID, &protocol.Control{
		Type:     protocol.TypeQuestion,
		ChatID:   chatID,
		Question: q,
	})
	ecancel()
	if err != nil {
		h.Answers.Cancel(q.RequestID)
		h.Logger.Warn("emit question card failed",
			log.FieldChatID, chatID,
			"request_id", q.RequestID,
			log.FieldError, err)
		return nil
	}
	h.Logger.Debug("askAndWait: card emitted, waiting for answer",
		log.FieldChatID, chatID,
		"prompt_id", promptID,
		"request_id", q.RequestID)
	select {
	case a, ok := <-ch:
		if !ok {
			// Broker drained on shutdown.
			h.Logger.Debug("askAndWait: channel closed",
				log.FieldChatID, chatID,
				"request_id", q.RequestID)
			return nil
		}
		h.Logger.Debug("askAndWait: answer received",
			log.FieldChatID, chatID,
			"request_id", q.RequestID)
		return a
	case <-ctx.Done():
		h.Logger.Debug("askAndWait: context cancelled",
			log.FieldChatID, chatID,
			"request_id", q.RequestID)
		h.Answers.Cancel(q.RequestID)
		return nil
	case <-time.After(bridgebase.AskWaitTimeout):
		h.Logger.Debug("askAndWait: timeout",
			log.FieldChatID, chatID,
			"request_id", q.RequestID)
		h.Answers.Cancel(q.RequestID)
		return nil
	}
}
