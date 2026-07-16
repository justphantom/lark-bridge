package goosebridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/bridgebase"
	"github.com/hu/lark-bridge/internal/goose"
	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/strutil"
)

// textEmitInterval bounds how often TypeText/TypeThinking deltas are
// forwarded to the frontend. Tool/result/error controls are always sent
// immediately.
const textEmitInterval = 200 * time.Millisecond

// streamRun consumes a goose event stream for one turn and translates each
// event into a protocol.Control emitted via h.emitAsync, while reducing the
// stream to a promptResult.
//
// goose stream-json emits thinking / text / toolRequest / toolResponse lines
// inside message blocks and one terminal complete line carrying token usage.
// There is no explicit session line: the session anchor (--name) is known up
// front from the binding, so the first successful turn back-fills it.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan goose.Event, modelSpec, sessionName string) promptResult {
	var (
		text      strings.Builder
		toolCount int
		startTime time.Time

		throttle = newControlThrottle(textEmitInterval)
	)

	// flushText is a no-op placeholder for symmetry with other bridges; goose
	// uses the shouldEmitText gate (drop-not-buffer) so there is no trailing
	// buffer to flush. Kept as an explicit empty func so the control-flow
	// reads identically across bridges.
	flushText := func() {}

	for ev := range events {
		h.logger.Debug("bridge received goose event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.GetType(),
			"text_length", len(ev.GetText()),
			log.FieldToolName, ev.GetToolName())

		if ctx.Err() != nil {
			flushText()
			return promptResult{
				err:         ctx.Err(),
				isCancelled: true,
				model:       resolveModel(modelSpec),
				sessionID:   sessionName,
			}
		}

		switch ev.GetType() {
		case goose.EventThinking:
			// Reasoning deltas stream live (not accumulated); gate by the same
			// throttle as text so a burst of one-char chunks does not flood IPC.
			if throttle.shouldEmitText(time.Now()) {
				h.emitAsync(promptID, &protocol.Control{
					Type:     protocol.TypeThinking,
					Thinking: &protocol.ThinkingPayload{Delta: ev.GetText()},
				})
			}
		case goose.EventText:
			text.WriteString(ev.GetText())
			if throttle.shouldEmitText(time.Now()) {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeText,
					Text: &protocol.TextPayload{Delta: ev.GetText()},
				})
			}
		case goose.EventToolUse:
			toolCount++
			if startTime.IsZero() {
				startTime = time.Now()
			}
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.GetToolName()},
			})
		case goose.EventToolResult:
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:    ev.GetToolName(),
					Output:  ev.GetText(),
					IsError: ev.GetIsToolError(),
				},
			})
		case goose.EventComplete:
			// Terminal success: back-fill the session anchor on the binding so
			// the next turn resumes, and fold the usage + accumulated text
			// into the result.
			if sessionName != "" {
				h.router.SetSessionID(chatID, sessionName)
			}
			return h.finalizeResult(text.String(), modelSpec, sessionName, toolCount, startTime, ev)
		case goose.EventError:
			h.logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", truncateForDebug(ev.GetText(), h.debugRedact()))
			return promptResult{
				err:       errors.New(nonEmpty(ev.GetText(), "goose 运行出错")),
				model:     resolveModel(modelSpec),
				sessionID: sessionName,
			}
		default:
			h.logger.Debug("goose: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.GetType())
		}
	}

	// Channel closed without a terminal event (defensive; the client normally
	// synthesizes an EventError when no complete arrived).
	if ctx.Err() != nil {
		return promptResult{
			err:         ctx.Err(),
			isCancelled: true,
			model:       resolveModel(modelSpec),
			sessionID:   sessionName,
		}
	}
	return promptResult{
		err:       errors.New("goose 流意外结束，未收到结果事件"),
		model:     resolveModel(modelSpec),
		sessionID: sessionName,
	}
}

// finalizeResult builds the promptResult from the accumulated text + the
// complete event's token usage. goose does not emit <think> tags in its text
// stream (reasoning arrives as separate thinking events), so StripThinking is
// a defensive no-op for any stray tags a model might inline.
func (h *Handler) finalizeResult(accText, modelSpec, sessionName string, toolCount int, startTime time.Time, ev goose.Event) promptResult {
	var durationMs int64
	if !startTime.IsZero() {
		durationMs = time.Since(startTime).Milliseconds()
	}

	result := promptResult{
		model:        resolveModel(modelSpec),
		sessionID:    sessionName,
		durationMs:   durationMs,
		steps:        toolCount,
		inputTokens:  ev.GetInputTokens(),
		outputTokens: ev.GetOutputTokens(),
	}

	reply := bridgebase.StripThinking(accText, "> ")
	// Empty-reply fallback: goose occasionally ends a turn with no text chunk
	// (e.g. the model only emitted tool calls whose results are already on the
	// progress card). When tools DID run, an absent summary is not an error.
	// When nothing ran, surface a retry hint so the user sees a signal.
	if strings.TrimSpace(reply) == "" && toolCount == 0 {
		reply = "（模型未返回内容，请重试或调整问题）"
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

func truncateForDebug(s string, redact bool) string {
	if redact {
		return "<redacted>"
	}
	return strutil.Truncate(s, maxDebugTextLen)
}

// resolveModel picks the model name for the result card. goose stream-json
// carries no model name on its lines, so when the user's modelSpec does not
// supply one, fall back to "goose".
func resolveModel(spec string) string {
	if spec != "" {
		return spec
	}
	return "goose"
}
