package opencodeservebridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencodeserve"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// textEmitInterval bounds how often TypeText/TypeThinking deltas are
// forwarded to the frontend. Tool/result/error controls are always sent
// immediately so the user sees them without delay.
const textEmitInterval = 200 * time.Millisecond

// streamRun consumes an opencode event stream for one turn and translates each
// event into a protocol.Control emitted via h.emit, while reducing the stream
// to a promptResult.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan opencodeserve.Event, modelSpec string) promptResult {
	var (
		text      strings.Builder
		sessionID string
		stepCount int
		startTime time.Time

		// accTokens/accCost accumulate every step_finish (tool-calls + stop) so
		// the usage store records the full turn total. opencode emits one
		// step_finish per agent step; only summing the terminal one lost the
		// intermediate steps' tokens and cost.
		accInput, accOutput, accCacheRead, accCacheWrite int
		accCost                                          float64

		throttle = bridgebase.NewControlThrottle(textEmitInterval)
	)

	for ev := range events {
		h.Logger.Debug("bridge received opencode event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.GetType(),
			log.FieldSessionID, ev.GetSessionID(),
			"text_length", len(ev.GetText()),
			log.FieldToolName, ev.GetToolName())

		if ctx.Err() != nil {
			return promptResult{
				err:         ctx.Err(),
				isCancelled: true,
				model:       resolveModel("", modelSpec),
				sessionID:   sessionID,
			}
		}

		// Only write the session id back when the chat is still bound: a
		// concurrent /session-del or /cd may have Unbound it, and a recreated
		// binding (new prompt) must not be clobbered with this turn's id.
		if sessionID == "" && ev.GetSessionID() != "" {
			sessionID = ev.GetSessionID()
			if _, ok := h.Router.Lookup(chatID); ok {
				h.Router.SetSessionID(chatID, sessionID)
			}
		}

		switch ev.GetType() {
		case opencodeserve.EventSession:
			if sessionID != "" {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeSessionInit,
					SessionInit: &protocol.SessionInitPayload{
						SessionID: sessionID,
						Model:     resolveModel("", modelSpec),
					},
				})
			}
		case opencodeserve.EventStepStart:
			stepCount++
			if startTime.IsZero() {
				startTime = time.Now()
			}
			h.emitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeProgress,
				ChatID: chatID,
				Progress: &protocol.ProgressPayload{
					Description: fmt.Sprintf("🔄 第 %d 轮…", stepCount),
				},
			})
		case opencodeserve.EventStepFinish:
			// Non-terminal step (reason != "stop"): accumulate its tokens and
			// cost so the usage store gets the full turn total. It produces no
			// card update — the progress card already shows the running step.
			accInput += ev.GetInputTokens()
			accOutput += ev.GetOutputTokens()
			accCacheRead += ev.GetCacheRead()
			accCacheWrite += ev.GetCacheWrite()
			accCost += ev.GetCost()
		case opencodeserve.EventText:
			text.WriteString(ev.GetText())
			if throttle.ShouldEmitText(time.Now()) {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeText,
					Text: &protocol.TextPayload{Delta: ev.GetText()},
				})
			}
		case opencodeserve.EventThinking:
			if throttle.ShouldEmitText(time.Now()) {
				h.emitAsync(promptID, &protocol.Control{
					Type:     protocol.TypeThinking,
					Thinking: &protocol.ThinkingPayload{Delta: ev.GetText()},
				})
			}
		case opencodeserve.EventToolUse:
			// opencode emits one completed event per call (parsed into
			// EventToolResult below), so this case is reached only if a
			// future CLI change reintroduces a separate use event. Kept for
			// forward-compat so the row still opens as running.
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.GetToolName(), Input: bridgebase.SummarizeToolInput(ev.GetToolInput())},
			})
		case opencodeserve.EventToolResult:
			// opencode's "task" tool IS the subagent delegation.
			isSub := ev.GetToolName() == "task"
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:       ev.GetToolName(),
					Input:      bridgebase.SummarizeToolInput(ev.GetToolInput()),
					Output:     ev.GetText(),
					IsError:    ev.GetIsToolError(),
					IsSubagent: isSub,
				},
			})
		case opencodeserve.EventResult:
			return h.finalizeResult(ev, text.String(), sessionID, modelSpec, chatID, stepCount, startTime,
				accInput, accOutput, accCacheRead, accCacheWrite, accCost)
		case opencodeserve.EventError:
			h.Logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", truncateForDebug(ev.GetText(), h.debugRedact()))
			return promptResult{
				err:       errors.New(nonEmpty(ev.GetText(), "opencode 运行出错")),
				model:     resolveModel("", modelSpec),
				sessionID: sessionID,
			}
		default:
			// Forward-compat: the parser forwards unknown line types verbatim.
			// Log at debug so a schema change is observable without breaking
			// the turn.
			h.Logger.Debug("opencode: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.GetType())
		}
	}

	// Channel closed without a terminal event (defensive; the client
	// normally synthesises an EventError). If the context was cancelled
	// (user abort or prompt timeout), surface it as a cancellation rather
	// than a generic error so emitTerminal shows the right notice.
	if ctx.Err() != nil {
		return promptResult{
			err:         ctx.Err(),
			isCancelled: true,
			model:       resolveModel("", modelSpec),
			sessionID:   sessionID,
		}
	}
	return promptResult{
		err:       errors.New("opencode 流意外结束，未收到结果事件"),
		model:     resolveModel("", modelSpec),
		sessionID: sessionID,
	}
}

// finalizeResult builds the promptResult from a result event.
func (h *Handler) finalizeResult(ev opencodeserve.Event, accText, sessionID, modelSpec, chatID string, stepCount int, startTime time.Time,
	accInput, accOutput, accCacheRead, accCacheWrite int, accCost float64) promptResult {
	var durationMs int64
	if !startTime.IsZero() {
		durationMs = time.Since(startTime).Milliseconds()
	}

	// Add the terminal (stop) step's tokens to the accumulated tool-calls
	// steps so the usage breakdown reflects the whole turn.
	totalInput := accInput + ev.GetInputTokens()
	totalOutput := accOutput + ev.GetOutputTokens()
	totalCacheRead := accCacheRead + ev.GetCacheRead()
	totalCacheWrite := accCacheWrite + ev.GetCacheWrite()

	result := promptResult{
		model:      resolveModel("", modelSpec),
		sessionID:  sessionID,
		durationMs: durationMs,
		// contextTokens stays terminal-step input+output (non-cache) so the
		// result card's token count remains claude-comparable and does not
		// jump when usage accounting started summing every step. The full
		// per-turn breakdown lives in inputTokens/outputTokens/cacheRead/
		// cacheWrite below for the usage store.
		contextTokens: ev.GetInputTokens() + ev.GetOutputTokens(),
		costUSD:       accCost + ev.GetCost(),
		steps:         stepCount,

		inputTokens:  totalInput,
		outputTokens: totalOutput,
		cacheRead:    totalCacheRead,
		cacheWrite:   totalCacheWrite,
	}

	if ev.GetIsError() {
		msg := ev.GetResult()
		if strings.TrimSpace(msg) == "" {
			msg = "opencode 返回错误"
		}
		result.err = errors.New(msg)
		return result
	}

	reply := ev.GetResult()
	if reply == "" {
		reply = bridgebase.StripThinking(accText, "> ")
	} else {
		reply = bridgebase.StripThinking(reply, "> ")
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

// resolveModel picks the model name for the result card. opencode serve
// events do not carry the model name on every frame, so when neither the
// stream nor the user's modelSpec supplies one, fall back to "opencode".
func resolveModel(_, spec string) string {
	if spec != "" {
		return spec
	}
	return "opencode"
}
