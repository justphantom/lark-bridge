package ompbridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/omp"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// errIdleTimeout is the cancel-cause set by the idle watchdog (see runPrompt)
// when no stdout event arrives within IdleTimeout. streamRun checks it via
// context.Cause to tell an idle kill apart from a user /session-abort, so
// emitTerminal can show "响应超时" vs "已取消".
var errIdleTimeout = errors.New("omp idle: no stdout event within idle_timeout")

// streamRun consumes an omp event stream for one turn and translates each
// event into a protocol.Control emitted via h.emit, while reducing the stream
// to a bridgebase.PromptResult. onActivity (nil disables the watchdog) is invoked once
// per received event so runPrompt can reset its idle timer.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan omp.Event, modelSpec string, onActivity func()) bridgebase.PromptResult {
	var (
		text      strings.Builder
		sessionID string
		stepCount int
		startTime time.Time

		// accTokens/accCost accumulate every role=assistant message_end so
		// the usage store records the full turn total. omp emits one
		// message_end per assistant message; agent_end carries no telemetry
		// (§A.1), so message_end is the sole source (§7.4).
		accInput, accOutput, accCacheRead, accCacheWrite int
		accCost                                          float64
	)

	for ev := range events {
		if onActivity != nil {
			onActivity()
		}
		h.Logger.Debug("bridge received omp event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.Type,
			log.FieldSessionID, ev.SessionID,
			"text_length", len(ev.Text),
			log.FieldToolName, ev.ToolName)

		if ctx.Err() != nil {
			idle := errors.Is(context.Cause(ctx), errIdleTimeout)
			return bridgebase.PromptResult{
				Err:           ctx.Err(),
				IsCancelled:   !idle,
				IsIdleTimeout: idle,
				Model:         bridgebase.ResolveModel("", modelSpec, "omp"),
				SessionID:     sessionID,
			}
		}

		// Only write the session id back when the chat is still bound: a
		// concurrent /session-del or /cd may have Unbound it, and a recreated
		// binding (new prompt) must not be clobbered with this turn's id.
		// The first `session` header line carries the id; back-fill it onto
		// the binding so the next turn --resume continues the session.
		if sessionID == "" && ev.SessionID != "" {
			sessionID = ev.SessionID
			if _, ok := h.Router.Lookup(chatID); ok {
				h.Router.SetSessionID(chatID, sessionID)
			}
			// Emit TypeSessionInit so the frontend footer renders Model +
			// SessionID instead of leaving those fields blank for the turn.
			h.EmitAsync(promptID, &protocol.Control{
				Type: protocol.TypeSessionInit,
				SessionInit: &protocol.SessionInitPayload{
					SessionID: sessionID,
					Model:     bridgebase.ResolveModel("", modelSpec, "omp"),
				},
			})
		}

		switch ev.Type {
		case omp.EventAgentStart:
			stepCount++
			// agent_start fires ONCE per turn (and once per auto_retry), NOT
			// once per assistant round. The per-round text reset happens on
			// EventTurnStart (verified against agnes-2.0-flash: a tool-call
			// turn emits agent_start → turn_start #1 → … → turn_start #2 →
			// …, and only resetting on turn_start discards round 1's
			// inline-thinking preamble correctly).
			if startTime.IsZero() {
				startTime = time.Now()
			}
			// Emit TypeProgress WITHOUT a Description: the dispatcher still
			// bumps stepCount (card title "第 N 轮"), but no banner is set —
			// the title already conveys the step, and a banner would
			// overwrite any standing gate/loading notice from a picker.
			h.EmitAsync(promptID, &protocol.Control{
				Type:     protocol.TypeProgress,
				ChatID:   chatID,
				Progress: &protocol.ProgressPayload{},
			})
		case omp.EventTurnStart:
			// New assistant round begins: discard any text accumulated in the
			// previous round. A tool-call turn streams one assistant message
			// per round — round N's preamble (for agnes/glm: inline thinking
			// text ending in a stray `</think>` before the toolcall) must not
			// be concatenated onto round N+1's final answer. StripThinking
			// cannot rescue this because OMP emits the closing `</think>`
			// without a matching open tag; turn_start is the only clean
			// boundary. (The first turn_start fires on an empty accumulator,
			// so the reset is harmless there.)
			text.Reset()
		case omp.EventMessageUpdate:
			text.WriteString(ev.Text)
		case omp.EventMessageEnd:
			// Usage (input/output/cacheRead/cacheWrite/cost) appears ONLY on
			// role=assistant message_end events (camelCase fields, §A.6);
			// role=toolResult message_end carries none. The parser fills
			// zeros for toolResult, and gates extraction on role=assistant,
			// so this accumulate is safe for both. agent_end has no
			// telemetry (§A.1), so this is the sole usage source.
			if ev.Role == "assistant" {
				accInput += ev.InputTokens
				accOutput += ev.OutputTokens
				accCacheRead += ev.CacheRead
				accCacheWrite += ev.CacheWrite
				accCost += ev.CostUSD
			}
		case omp.EventThinking:
			// omp streams thinking_delta increments; thinking_end carries the
			// whole block. Replace=true lets the renderer's thinking zone
			// reflect the latest state instead of concatenating every chunk
			// into an unreadable wall (claude/opencode parity).
			h.EmitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeThinking,
				ChatID: chatID,
				Thinking: &protocol.ThinkingPayload{
					Delta:   ev.Text,
					Replace: true,
				},
			})
		case omp.EventToolStart:
			h.EmitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{
					Name:  ev.ToolName,
					Input: bridgebase.SummarizeToolInput(ev.ToolName, ev.ToolInput),
				},
			})
		case omp.EventToolEnd:
			h.EmitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:    ev.ToolName,
					Output:  ev.ToolOutput,
					IsError: ev.IsToolError,
				},
			})
		case omp.EventAutoRetry:
			// omp began an automatic retry; surface it so the card is not
			// silent during the retry window. The turn continues (auto_retry
			// is non-terminal).
			h.EmitAsync(promptID, &protocol.Control{
				Type:     protocol.TypeProgress,
				ChatID:   chatID,
				Progress: &protocol.ProgressPayload{Description: fmt.Sprintf("自动重试 #%d", ev.Attempt)},
			})
		case omp.EventAgentEnd:
			return h.finalizeResult(text.String(), sessionID, modelSpec, chatID, stepCount, startTime,
				accInput, accOutput, accCacheRead, accCacheWrite, accCost)
		case omp.EventError:
			h.Logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", bridgebase.TruncateForDebug(ev.Text, h.DebugRedact()))
			return bridgebase.PromptResult{
				Err:       errors.New(bridgebase.NonEmpty(ev.Text, "OMP 运行出错")),
				Model:     bridgebase.ResolveModel("", modelSpec, "omp"),
				SessionID: sessionID,
			}
		default:
			// Forward-compat: the parser forwards unknown line types verbatim.
			// Log at debug so a schema change is observable without breaking
			// the turn.
			h.Logger.Debug("omp: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, ev.Type)
		}
	}

	// Channel closed without a terminal event (defensive; the client
	// normally synthesises an EventError). If the context was cancelled
	// (user abort or prompt timeout), surface it as a cancellation rather
	// than a generic error so emitTerminal shows the right notice.
	if ctx.Err() != nil {
		idle := errors.Is(context.Cause(ctx), errIdleTimeout)
		return bridgebase.PromptResult{
			Err:           ctx.Err(),
			IsCancelled:   !idle,
			IsIdleTimeout: idle,
			Model:         bridgebase.ResolveModel("", modelSpec, "omp"),
			SessionID:     sessionID,
		}
	}
	return bridgebase.PromptResult{
		Err:       errors.New("omp 流意外结束，未收到结果事件"),
		Model:     bridgebase.ResolveModel("", modelSpec, "omp"),
		SessionID: sessionID,
	}
}

// finalizeResult builds the bridgebase.PromptResult from an agent_end event per §7.4.
// agent_end carries no telemetry (§A.1), so usage is read entirely from the
// acc* accumulators (filled by the EventMessageEnd case in streamRun). The
// terminal round's text is the accumulated assistant text.
func (h *Handler) finalizeResult(accText, sessionID, modelSpec, chatID string, stepCount int, startTime time.Time,
	accInput, accOutput, accCacheRead, accCacheWrite int, accCost float64) bridgebase.PromptResult {
	var durationMs int64
	if !startTime.IsZero() {
		durationMs = time.Since(startTime).Milliseconds()
	}

	result := bridgebase.PromptResult{
		Model:      bridgebase.ResolveModel("", modelSpec, "omp"),
		SessionID:  sessionID,
		DurationMs: durationMs,
		// contextTokens = accInput + accOutput (non-cache), aligned with the
		// claude/opencode result card so the token count stays comparable
		// across backends.
		ContextToken: accInput + accOutput,
		CostUSD:      accCost,
		Steps:        stepCount,

		InputTokens:  accInput,
		OutputTokens: accOutput,
		CacheRead:    accCacheRead,
		CacheWrite:   accCacheWrite,
	}

	reply := bridgebase.StripThinking(accText, "> ")
	result.Reply = reply
	return result
}

// maxDebugTextLen / nonEmpty / truncateForDebug / resolveModel were local
// copies of helpers now shared from package bridgebase (util.go).
