package peribridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/log"
	"github.com/hu/lark-bridge/internal/peri"
	"github.com/hu/lark-bridge/internal/protocol"
	"github.com/hu/lark-bridge/internal/strutil"
)

// textEmitInterval bounds how often TypeText/TypeThinking deltas are
// forwarded to the frontend. Tool/result/error controls are always sent
// immediately.
const textEmitInterval = 200 * time.Millisecond

// streamRun consumes a peri event stream for one turn and translates each
// event into a protocol.Control emitted via h.emitAsync, while reducing the
// stream to a promptResult.
//
// peri stream-json emits only text / tool_use / tool_result lines and has no
// session, step, or usage events. tool_use count stands in for "steps" on the
// result card; tokens/cost are always zero (peri emits no usage).
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan peri.Event, modelSpec string) promptResult {
	var (
		text      strings.Builder
		toolCount int
		startTime time.Time

		throttle = newControlThrottle(textEmitInterval)
	)

	for ev := range events {
		h.logger.Debug("bridge received peri event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.GetType(),
			"text_length", len(ev.GetText()),
			log.FieldToolName, ev.GetToolName())

		if ctx.Err() != nil {
			return promptResult{
				err:         ctx.Err(),
				isCancelled: true,
				model:       resolveModel(modelSpec),
			}
		}

		switch ev.GetType() {
		case peri.EventText:
			text.WriteString(ev.GetText())
			if throttle.shouldEmitText(time.Now()) {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeText,
					Text: &protocol.TextPayload{Delta: ev.GetText()},
				})
			}
		case peri.EventToolUse:
			toolCount++
			if startTime.IsZero() {
				startTime = time.Now()
			}
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.GetToolName()},
			})
		case peri.EventToolResult:
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:    ev.GetToolName(),
					Output:  ev.GetText(),
					IsError: ev.GetIsToolError(),
				},
			})
		case peri.EventResult:
			return h.finalizeResult(ev, text.String(), modelSpec, toolCount, startTime)
		case peri.EventError:
			h.logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", truncateForDebug(ev.GetText(), h.debugRedact()))
			return promptResult{
				err:   errors.New(nonEmpty(ev.GetText(), "peri 运行出错")),
				model: resolveModel(modelSpec),
			}
		default:
			h.logger.Debug("peri: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.GetType())
		}
	}

	// Channel closed without a terminal event (defensive; the client normally
	// synthesizes an EventError). If the context was cancelled, surface it as
	// a cancellation.
	if ctx.Err() != nil {
		return promptResult{
			err:         ctx.Err(),
			isCancelled: true,
			model:       resolveModel(modelSpec),
		}
	}
	return promptResult{
		err:   errors.New("peri 流意外结束，未收到结果事件"),
		model: resolveModel(modelSpec),
	}
}

// finalizeResult builds the promptResult from a result event. peri's result is
// synthesized from accumulated text by the client, so it should match the
// locally accumulated text; prefer the event's result (post-strip) but fall
// back to the local accumulator if the event is empty.
func (h *Handler) finalizeResult(ev peri.Event, accText, modelSpec string, toolCount int, startTime time.Time) promptResult {
	var durationMs int64
	if !startTime.IsZero() {
		durationMs = time.Since(startTime).Milliseconds()
	}

	result := promptResult{
		model:      resolveModel(modelSpec),
		durationMs: durationMs,
		steps:      toolCount,
	}

	reply := ev.GetResult()
	if strings.TrimSpace(reply) == "" {
		reply = stripThinking(accText)
	} else {
		reply = stripThinking(reply)
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

// resolveModel picks the model name for the result card. peri stream-json
// carries no model name, so when the user's modelSpec does not supply one,
// fall back to "peri".
func resolveModel(spec string) string {
	if spec != "" {
		return spec
	}
	return "peri"
}

// summarizeToolInput is retained for structural parity with the opencode
// bridge but peri stream-json emits null tool input, so it always returns "".
// Kept as a documented no-op so callers reading the opencode bridge do not
// expect a missing helper here.
func summarizeToolInput(input string) string {
	if input == "" || input == "{}" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		return input
	}
	for _, key := range []string{"command", "filePath", "pattern", "path", "query", "description", "prompt", "url"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	for _, v := range m {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return input
}
