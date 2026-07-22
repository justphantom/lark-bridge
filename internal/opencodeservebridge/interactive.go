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
	label := "opencode 请求权限：" + p.Permission
	if len(p.Patterns) > 0 {
		label += "\n" + strings.Join(p.Patterns, "\n")
	}
	opts := make([]string, 0, len(permissionOptions))
	for _, o := range permissionOptions {
		opts = append(opts, o.label)
	}
	ans := h.askAndWait(ctx, chatID, promptID, &protocol.QuestionPayload{
		RequestID: p.ID,
		PromptID:  promptID,
		Questions: []protocol.QuestionItem{{Label: label, Options: opts}},
	})
	reply := oc.PermissionReplyReject
	if ans != nil {
		reply = permissionReplyOf(bridgebase.PickAnswerValue(ans))
	}
	rctx, cancel := context.WithTimeout(context.Background(), interactiveOpTimeout)
	defer cancel()
	if err := h.agent.ReplyPermission(rctx, p.ID, reply, ""); err != nil {
		h.Logger.Warn("reply permission failed",
			log.FieldChatID, chatID,
			"request_id", p.ID,
			log.FieldError, err)
		return
	}
	h.Logger.Debug("permission replied",
		log.FieldChatID, chatID,
		"request_id", p.ID,
		"reply", reply)
	// Echo the answer onto the progress card so the user can see what was
	// answered without scrolling back to the standalone permission card.
	if ans != nil {
		if summary := bridgebase.PickAnswerValue(ans); summary != "" {
			h.emitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeText,
				ChatID: chatID,
				Text:   &protocol.TextPayload{Delta: "✓ 已应答权限请求: " + summary + "\n"},
			})
		}
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
	rctx, cancel := context.WithTimeout(context.Background(), interactiveOpTimeout)
	defer cancel()
	reply, ok := questionReplyFromAnswer(q, ans)
	var err error
	if !ok {
		err = h.agent.RejectQuestion(rctx, q.ID)
	} else {
		err = h.agent.ReplyQuestion(rctx, q.ID, reply)
	}
	if err != nil {
		h.Logger.Warn("reply question failed",
			log.FieldChatID, chatID,
			"request_id", q.ID,
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
	ch, ok := h.Answers.Register(q.RequestID)
	if !ok {
		h.Logger.Warn("duplicate interactive request",
			log.FieldChatID, chatID,
			"request_id", q.RequestID)
		return nil
	}
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
	select {
	case a, ok := <-ch:
		if !ok {
			// Broker drained on shutdown.
			return nil
		}
		return a
	case <-ctx.Done():
		h.Answers.Cancel(q.RequestID)
		return nil
	case <-time.After(bridgebase.AskWaitTimeout):
		h.Answers.Cancel(q.RequestID)
		return nil
	}
}
