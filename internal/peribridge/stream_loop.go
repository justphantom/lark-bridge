package peribridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/bridgebase"
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

		throttle = newTextThrottle(textEmitInterval)
	)

	// flushText drains any buffered text delta. Called before tool/error/result
	// events so a pending partial batch is delivered rather than lost or
	// interleaved with non-text controls. A no-op when the buffer is empty.
	flushText := func() {
		if delta := throttle.Flush(); delta != "" {
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeText,
				Text: &protocol.TextPayload{Delta: delta},
			})
		}
	}

	for ev := range events {
		h.logger.Debug("bridge received peri event",
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
			}
		}

		switch ev.GetType() {
		case peri.EventText:
			text.WriteString(ev.GetText())
			// Batch this chunk with any pending ones; Add returns the merged
			// batch when the interval elapses, "" to keep buffering.
			if delta := throttle.Add(ev.GetText(), time.Now()); delta != "" {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeText,
					Text: &protocol.TextPayload{Delta: delta},
				})
			}
		case peri.EventToolUse:
			flushText()
			toolCount++
			if startTime.IsZero() {
				startTime = time.Now()
			}
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.GetToolName()},
			})
		case peri.EventToolResult:
			flushText()
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:    ev.GetToolName(),
					Output:  ev.GetText(),
					IsError: ev.GetIsToolError(),
				},
			})
		case peri.EventResult:
			flushText()
			return h.finalizeResult(ev, text.String(), modelSpec, toolCount, startTime)
		case peri.EventError:
			flushText()
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
		reply = bridgebase.StripThinking(accText, "> ")
	} else {
		reply = bridgebase.StripThinking(reply, "> ")
	}
	// Empty-reply fallback: peri print mode (stream-json) ends with no terminal
	// result line, and the model occasionally returns an empty content (no
	// text, no tool call). Without this the user sees a blank result card with
	// no signal to retry. When tools DID run, their results are already on the
	// progress card, so an absent final summary is not surfaced as an error.
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

// resolveModel picks the model name for the result card. peri stream-json
// carries no model name, so when the user's modelSpec does not supply one,
// fall back to "peri".
func resolveModel(spec string) string {
	if spec != "" {
		return spec
	}
	return "peri"
}
