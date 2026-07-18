package claudebridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/claude"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// textEmitInterval bounds how often TypeText/TypeThinking deltas are
// forwarded to the frontend. Tool/result/error controls are always sent
// immediately so the user sees them without delay.
const textEmitInterval = 200 * time.Millisecond

// streamRun consumes a Claude event stream for one turn and translates each
// event into a protocol.Control emitted via h.emit, while reducing the stream
// to a promptResult.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan claude.Event, modelSpec string) promptResult {
	var (
		text      strings.Builder
		sessionID string
		model     string

		// toolNames correlates a tool_use id to its name so the matching
		// tool_result (which carries only the id, not the name) can be
		// rendered with the right tool row in the progress card.
		toolNames = map[string]string{}

		throttle = bridgebase.NewControlThrottle(textEmitInterval)
	)

	for ev := range events {
		h.Logger.Debug("bridge received claude event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.GetType(),
			log.FieldEventSubtype, ev.GetSubtype(),
			log.FieldSessionID, ev.GetSessionID(),
			"text_length", len(ev.GetText()),
			log.FieldToolName, ev.GetToolName())

		// Stop early once the turn is cancelled.
		if ctx.Err() != nil {
			return promptResult{
				err:         ctx.Err(),
				isCancelled: true,
				model:       firstNonEmpty(model, modelSpec),
				sessionID:   sessionID,
			}
		}

		// Capture session id from system/init (before emitting so the binding
		// is updated regardless of downstream state). Guard against a concurrent
		// /session-del or /cd that may have Unbound this chat between turn
		// start and now: SetSessionID on a removed binding is a no-op, but on a
		// freshly recreated binding (a new prompt sneaking in) it would clobber
		// that new prompt's empty sessionID — so only write when the chat is
		// still bound.
		if sessionID == "" && ev.GetSessionID() != "" {
			sessionID = ev.GetSessionID()
			if _, ok := h.Router.Lookup(chatID); ok {
				h.Router.SetSessionID(chatID, sessionID)
			}
			h.Logger.Debug("captured claude session id",
				log.FieldChatID, chatID,
				log.FieldSessionID, sessionID)
		}
		if model == "" && ev.GetModel() != "" {
			model = ev.GetModel()
		}

		switch ev.GetType() {
		case claude.EventSystem:
			// Only init is actionable (carries session id + model). Other
			// system subtypes — chiefly thinking_tokens (the bulk of the
			// stream), but also any future internal signal — are ignored by
			// falling through this case to the loop.
			if ev.GetSubtype() == claude.SubtypeInit && sessionID != "" {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeSessionInit,
					SessionInit: &protocol.SessionInitPayload{
						SessionID: sessionID,
						Model:     firstNonEmpty(model, modelSpec),
					},
				})
			}
		case claude.EventTaskStarted:
			// A subagent (Task/Agent tool) spawned. Surface it as a fresh
			// running tool row named after the subagent type. TaskID lets the
			// frontend fold this row with its later progress/notification even
			// though name/desc drift across the lifecycle.
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: taskToolName(ev.GetTaskType(), ev.GetTaskKind()), Input: ev.GetTaskDesc(), IsSubagent: true, TaskID: ev.GetTaskID()},
			})
		case claude.EventTaskProgress:
			// Live subagent progress: re-emit as a ToolUse so the existing
			// same-TaskID row updates its description while staying running.
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: taskToolName(ev.GetTaskType(), ev.GetTaskKind()), Input: taskProgressDesc(ev), IsSubagent: true, TaskID: ev.GetTaskID()},
			})
		case claude.EventTaskNotification:
			// Subagent finished: close the running row by TaskID. The terminal
			// summary (title + cumulative usage) rides on Input so it lands in
			// the tool-row description; the progress card shows actions, not
			// tool output, so Output is left empty.
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:       taskToolName(ev.GetTaskType(), ev.GetTaskKind()),
					Input:      taskProgressDesc(ev),
					IsError:    ev.GetIsToolError(),
					IsSubagent: true,
					TaskID:     ev.GetTaskID(),
				},
			})
		case claude.EventText:
			text.WriteString(ev.GetText())
			if throttle.ShouldEmitText(time.Now()) {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeText,
					Text: &protocol.TextPayload{Delta: ev.GetText()},
				})
			}
		case claude.EventThinking:
			if throttle.ShouldEmitText(time.Now()) {
				h.emitAsync(promptID, &protocol.Control{
					Type:     protocol.TypeThinking,
					Thinking: &protocol.ThinkingPayload{Delta: ev.GetText()},
				})
			}
		case claude.EventToolUse:
			if id := ev.GetToolID(); id != "" {
				toolNames[id] = ev.GetToolName()
			}
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.GetToolName(), Input: bridgebase.SummarizeToolInput(ev.GetToolInput())},
			})
		case claude.EventToolResult:
			// claude tool_result carries only the id; look up the name
			// recorded at tool_use time so the card can match the row.
			name := ev.GetToolName()
			if name == "" {
				name = toolNames[ev.GetToolID()]
			}
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:    name,
					Output:  ev.GetText(),
					IsError: ev.GetIsToolError(),
				},
			})
		case claude.EventResult:
			return h.finalizeResult(ev, text.String(), sessionID, model, modelSpec, chatID)
		case claude.EventError:
			h.Logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", truncateForDebug(ev.GetText(), h.debugRedact()))
			return promptResult{
				err:       errors.New(nonEmpty(ev.GetText(), "Claude 运行出错")),
				model:     firstNonEmpty(model, modelSpec),
				sessionID: sessionID,
			}
		default:
			// Forward-compat: the parser forwards unknown line types verbatim
			// (raw retained). Log at debug so a schema change is observable
			// without dropping the event silently or breaking the turn.
			h.Logger.Debug("claude: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.GetType(),
				log.FieldEventSubtype, ev.GetSubtype())
		}
	}

	// Channel closed without a terminal event (defensive; the client normally
	// synthesises an EventError). If the context was cancelled (user abort
	// or prompt timeout), surface it as a cancellation rather than a generic
	// error so emitTerminal shows the right notice.
	if ctx.Err() != nil {
		return promptResult{
			err:         ctx.Err(),
			isCancelled: true,
			model:       firstNonEmpty(model, modelSpec),
			sessionID:   sessionID,
		}
	}
	return promptResult{
		err:       errors.New("claude 流意外结束，未收到结果事件"),
		model:     firstNonEmpty(model, modelSpec),
		sessionID: sessionID,
	}
}

// finalizeResult builds the promptResult from a result event. The reply
// comes from the result event's result field (the protocol truth), falling
// back to accumulated text blocks.
func (h *Handler) finalizeResult(ev claude.Event, accText, sessionID, model, modelSpec, chatID string) promptResult {
	h.Logger.Debug("bridge: result event",
		log.FieldChatID, chatID,
		"is_error", ev.GetIsError(),
		"cost_usd", ev.GetCostUSD(),
		log.FieldDuration, ev.GetDurationMs(),
		log.FieldModel, firstNonEmpty(model, modelSpec),
		"result_preview", truncateForDebug(ev.GetResult(), h.debugRedact()))

	result := promptResult{
		model:         firstNonEmpty(model, modelSpec),
		sessionID:     sessionID,
		durationMs:    ev.GetDurationMs(),
		contextTokens: ev.GetInputTokens() + ev.GetOutputTokens(),
		costUSD:       ev.GetCostUSD(),
		steps:         ev.GetNumTurns(),

		inputTokens:   ev.GetInputTokens(),
		outputTokens:  ev.GetOutputTokens(),
		cacheRead:     ev.GetCacheRead(),
		cacheCreation: ev.GetCacheCreation(),
	}

	if ev.GetIsError() {
		msg := ev.GetResult()
		if strings.TrimSpace(msg) == "" {
			msg = "Claude 返回错误"
		}
		result.err = errors.New(msg)
		return result
	}

	reply := ev.GetResult()
	if reply == "" {
		reply = bridgebase.StripThinking(accText, "> 💭 ")
	} else {
		reply = bridgebase.StripThinking(reply, "> 💭 ")
	}
	result.reply = reply
	return result
}

// maxDebugTextLen caps the preview length used in debug logs.
const maxDebugTextLen = 200

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// truncateForDebug returns a string for debug logging: optionally redacted
// (replaced wholesale) and always truncated to a bounded length.
func truncateForDebug(s string, redact bool) string {
	if redact {
		return "<redacted>"
	}
	return strutil.Truncate(s, maxDebugTextLen)
}

// firstNonEmpty returns the first non-empty string, or "" if all are empty.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
