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

// This file holds the helpers that support handlePermissionAsked /
// handleQuestionAsked (the entry points kept in interactive.go):
//
//   - gate banner shaping + structured permission body (Type/Title/Detail), so
//     the renderer can show a blocking banner on the progress card and a typed
//     permission card;
//   - the ask lifecycle (directoryOf / askAndWait) and the answer→serve-reply
//     mapping (questionReplyFromAnswer).
//
// Presentation lives in the renderer; these helpers only shape data and drive
// the register→emit→wait→reply loop.

// gateControl builds a TypeProgress control carrying a gate banner, used to
// surface an interactive gate's state on the streaming progress card. EmitAsync
// stamps PromptID, so callers pass promptID positionally and leave the
// control's PromptID empty.
func gateControl(chatID, promptID, kind, state, summary string) *protocol.Control {
	return &protocol.Control{
		Type:   protocol.TypeProgress,
		ChatID: chatID,
		Progress: &protocol.ProgressPayload{
			Gate: &protocol.GateInfo{State: state, Kind: kind, Summary: summary},
		},
	}
}

// permissionLabel is the legacy flat body (kept as the renderer fallback for
// older frontends that do not understand the structured Type/Title/Detail).
func permissionLabel(p *oc.PermissionAskedData) string {
	label := "opencode 请求权限：" + p.Permission
	if len(p.Patterns) > 0 {
		label += "\n" + strings.Join(p.Patterns, "\n")
	}
	return label
}

// permissionTitle returns the one-line headline for a structured permission
// card: the first pattern (the command or target file) when present, else the
// permission category.
func permissionTitle(p *oc.PermissionAskedData) string {
	if len(p.Patterns) > 0 && p.Patterns[0] != "" {
		return p.Patterns[0]
	}
	return p.Permission
}

// permissionMessage builds the structured body carrier: Type is the permission
// category, Title the headline, Detail the full patterns block, Message the
// flat fallback. All four are populated so a renderer may use either form.
func permissionMessage(p *oc.PermissionAskedData) protocol.PermissionMessage {
	return protocol.PermissionMessage{
		Message: permissionLabel(p),
		Type:    p.Permission,
		Title:   permissionTitle(p),
		Detail:  strings.Join(p.Patterns, "\n"),
	}
}

// gatePermissionSummary is the short banner text for the waiting state:
// category plus the first pattern (command/file) so the user sees what is
// being authorized without opening the standalone card.
func gatePermissionSummary(p *oc.PermissionAskedData) string {
	if t := permissionTitle(p); t != "" && t != p.Permission {
		return p.Permission + " · " + t
	}
	return p.Permission
}

// gateQuestionSummary is the short banner text for the waiting state: the
// first question's label so the user sees what the agent is asking.
func gateQuestionSummary(q *oc.QuestionAskedData) string {
	if len(q.Questions) > 0 && q.Questions[0].Question != "" {
		return q.Questions[0].Question
	}
	return ""
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
//
// Multi-question + Custom is intercepted by the caller (handleQuestionAsked)
// before this function runs, so the len(answers)>1 && Custom!="" branch
// below is exercised only on the single-question path it was designed for.
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
