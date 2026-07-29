package opencodebridge

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/opencode"
	"github.com/justphantom/lark-bridge/internal/protocol"
)

// errIdleTimeout is the cancel-cause set by the idle watchdog (see
// runPrompt) when no stdout event arrives within IdleTimeout. streamRun
// checks it via context.Cause to tell an idle kill apart from a user
// /session-abort, so emitTerminal can show "响应超时" vs "已取消".
var errIdleTimeout = errors.New("opencode idle: no stdout event within idle_timeout")

// streamRun consumes an opencode event stream for one turn and translates each
// event into a protocol.Control emitted via h.emit, while reducing the stream
// to a bridgebase.PromptResult. onActivity (nil disables the watchdog) is invoked once
// per received event so runPrompt can reset its idle timer.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan opencode.Event, modelSpec string, onActivity func()) bridgebase.PromptResult {
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
	)

	for ev := range events {
		if onActivity != nil {
			onActivity()
		}
		h.Logger.Debug("bridge received opencode event",
			log.FieldChatID, chatID,
			log.FieldEventType, ev.GetType(),
			log.FieldSessionID, ev.GetSessionID(),
			"text_length", len(ev.GetText()),
			log.FieldToolName, ev.GetToolName())

		if ctx.Err() != nil {
			idle := errors.Is(context.Cause(ctx), errIdleTimeout)
			return bridgebase.PromptResult{
				Err:           ctx.Err(),
				IsCancelled:   !idle,
				IsIdleTimeout: idle,
				Model:         bridgebase.ResolveModel("", modelSpec, "opencode"),
				SessionID:     sessionID,
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
			// opencode CLI 1.18+ does not emit session.created in --format
			// json mode (the sessionID rides on every part instead), so the
			// dead EventSession case below never fires. Synthesising the
			// TypeSessionInit here on first sight of the id lets the
			// frontend footer render Model + SessionID instead of leaving
			// those fields blank for the whole turn.
			h.EmitAsync(promptID, &protocol.Control{
				Type: protocol.TypeSessionInit,
				SessionInit: &protocol.SessionInitPayload{
					SessionID: sessionID,
					Model:     bridgebase.ResolveModel("", modelSpec, "opencode"),
				},
			})
		}

		switch ev.GetType() {
		case opencode.EventStepStart:
			stepCount++
			// A new agent step begins: discard any text accumulated in the
			// previous step. opencode emits one assistant text part per step
			// (the preamble before a tool call in step N, the final answer
			// after the last tool call in step N+1); the reply must be only
			// the terminal step's text. Without this reset the step-N
			// preamble gets concatenated onto the final answer.
			text.Reset()
			if startTime.IsZero() {
				startTime = time.Now()
			}
			// Emit TypeProgress WITHOUT a Description: dispatcher still
			// bumps stepCount (so the card title shows "第 N 轮"), but no
			// banner is set — the title already conveys the step, and a
			// banner here would duplicate it as well as overwrite any
			// standing gate/loading notice from a picker or permission
			// card on the same prompt.
			h.EmitAsync(promptID, &protocol.Control{
				Type:     protocol.TypeProgress,
				ChatID:   chatID,
				Progress: &protocol.ProgressPayload{},
			})
		case opencode.EventStepFinish:
			// Non-terminal step (reason != "stop"): accumulate its tokens and
			// cost so the usage store gets the full turn total. It produces no
			// card update — the progress card already shows the running step.
			accInput += ev.GetInputTokens()
			accOutput += ev.GetOutputTokens()
			accCacheRead += ev.GetCacheRead()
			accCacheWrite += ev.GetCacheWrite()
			accCost += ev.GetCost()
		case opencode.EventText:
			text.WriteString(ev.GetText())
		case opencode.EventThinking:
			// opencode emits one reasoning event per part carrying the
			// complete block (not a delta), so Replace=true lets the
			// renderer's thinking zone reflect the latest part instead of
			// concatenating every step's reasoning into an unreadable wall.
			h.EmitAsync(promptID, &protocol.Control{
				Type:   protocol.TypeThinking,
				ChatID: chatID,
				Thinking: &protocol.ThinkingPayload{
					Delta:   ev.GetText(),
					Replace: true,
				},
			})
		case opencode.EventToolUse:
			// opencode emits one completed event per call (parsed into
			// EventToolResult below), so this case is reached only if a
			// future CLI change reintroduces a separate use event. Kept for
			// forward-compat so the row still opens as running.
			h.EmitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.GetToolName(), Input: bridgebase.SummarizeToolInput(ev.GetToolName(), ev.GetToolInput())},
			})
		case opencode.EventToolResult:
			// todowrite carries the session's full todo list as input
			// (`{"todos":[...]}`), whose shape is identical to
			// protocol.TodoItem. Route it to TypeTodo so the progress card's
			// todo zone renders ✅/⏳/⬜/✘ rows instead of a raw-JSON tool
			// output. Single-sends (no parallel TypeToolResult) keep the card
			// clean: the timeline anchor ("model updated the list at step N")
			// is implicit in the step_count banner. A failed todowrite or an
			// unparseable input falls through to the generic tool row so the
			// failure is still visible.
			if ev.GetToolName() == "todowrite" && !ev.GetIsToolError() {
				if items, ok := bridgebase.ParseTodoItems(ev.GetToolInput()); ok {
					h.EmitAsync(promptID, &protocol.Control{
						Type:   protocol.TypeTodo,
						ChatID: chatID,
						Todo:   &protocol.TodoPayload{Todos: items},
					})
					continue
				}
			}
			//opencode's "task" tool IS the subagent delegation.
			isSub := ev.GetToolName() == "task"
			toolResult := &protocol.ToolResultPayload{
				Name:       ev.GetToolName(),
				Input:      bridgebase.SummarizeToolInput(ev.GetToolName(), ev.GetToolInput()),
				Output:     ev.GetText(),
				IsError:    ev.GetIsToolError(),
				IsSubagent: isSub,
			}
			// When the parser extracted SubagentMeta (task tool only), lift it
			// into the payload so the renderer routes the row to the dedicated
			// subagent zone instead of the leaf-tool completed zone. Preview
			// carries the unwrapped output verbatim; the renderer caps it.
			if meta := ev.GetSubagentMeta(); isSub && meta != nil {
				preview := opencode.UnwrapTaskResult(ev.GetText())
				status := "completed"
				if ev.GetIsToolError() {
					status = "failed"
				}
				toolResult.TaskID = meta.ChildSession
				toolResult.Subagent = &protocol.SubagentSummary{
					Status:       status,
					TaskType:     "agent",
					Type:         meta.Type,
					Title:        toolResult.Input,
					ChildSession: meta.ChildSession,
					Model:        meta.Model,
					DurationMs:   meta.DurationMs,
					Preview:      preview,
					OutputBytes:  meta.OutputBytes,
					Truncated:    meta.Truncated,
				}
			}
			h.EmitAsync(promptID, &protocol.Control{
				Type:       protocol.TypeToolResult,
				ToolResult: toolResult,
			})
		case opencode.EventResult:
			return h.finalizeResult(ev, text.String(), sessionID, modelSpec, chatID, stepCount, startTime,
				accInput, accOutput, accCacheRead, accCacheWrite, accCost)
		case opencode.EventError:
			h.Logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", bridgebase.TruncateForDebug(ev.GetText(), h.DebugRedact()))
			return bridgebase.PromptResult{
				Err:       errors.New(bridgebase.NonEmpty(ev.GetText(), "opencode 运行出错")),
				Model:     bridgebase.ResolveModel("", modelSpec, "opencode"),
				SessionID: sessionID,
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
		idle := errors.Is(context.Cause(ctx), errIdleTimeout)
		return bridgebase.PromptResult{
			Err:           ctx.Err(),
			IsCancelled:   !idle,
			IsIdleTimeout: idle,
			Model:         bridgebase.ResolveModel("", modelSpec, "opencode"),
			SessionID:     sessionID,
		}
	}
	return bridgebase.PromptResult{
		Err:       errors.New("opencode 流意外结束，未收到结果事件"),
		Model:     bridgebase.ResolveModel("", modelSpec, "opencode"),
		SessionID: sessionID,
	}
}

// finalizeResult builds the bridgebase.PromptResult from a result event.
func (h *Handler) finalizeResult(ev opencode.Event, accText, sessionID, modelSpec, chatID string, stepCount int, startTime time.Time,
	accInput, accOutput, accCacheRead, accCacheWrite int, accCost float64) bridgebase.PromptResult {
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

	result := bridgebase.PromptResult{
		Model:      bridgebase.ResolveModel("", modelSpec, "opencode"),
		SessionID:  sessionID,
		DurationMs: durationMs,
		// contextTokens stays terminal-step input+output (non-cache) so the
		// result card's token count remains claude-comparable and does not
		// jump when usage accounting started summing every step. The full
		// per-turn breakdown lives in inputTokens/outputTokens/cacheRead/
		// cacheWrite below for the usage store.
		ContextToken: ev.GetInputTokens() + ev.GetOutputTokens(),
		CostUSD:      accCost + ev.GetCost(),
		Steps:        stepCount,

		InputTokens:  totalInput,
		OutputTokens: totalOutput,
		CacheRead:    totalCacheRead,
		CacheWrite:   totalCacheWrite,
	}

	if ev.GetIsError() {
		msg := ev.GetResult()
		if strings.TrimSpace(msg) == "" {
			msg = "opencode 返回错误"
		}
		result.Err = errors.New(msg)
		return result
	}

	reply := ev.GetResult()
	if reply == "" {
		reply = bridgebase.StripThinking(accText, "> ")
	} else {
		reply = bridgebase.StripThinking(reply, "> ")
	}
	result.Reply = reply
	return result
}

// maxDebugTextLen / nonEmpty / truncateForDebug / resolveModel were local
// copies of helpers now shared from package bridgebase (util.go).
