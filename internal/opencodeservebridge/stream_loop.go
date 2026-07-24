package opencodeservebridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	oc "github.com/justphantom/opencode-go-sdk-lite"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/log"
	"github.com/justphantom/lark-bridge/internal/protocol"
	"github.com/justphantom/lark-bridge/internal/strutil"
)

// streamRun consumes an SDK HighEvent stream for one turn and translates each
// event into a protocol.Control emitted via h.emit, while reducing the stream
// to a promptResult.
func (h *Handler) streamRun(ctx context.Context, chatID, promptID string, events <-chan oc.HighEvent, modelSpec string) promptResult {
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
		h.Logger.Debug("bridge received opencode event",
			log.FieldChatID, chatID,
			log.FieldEventType, string(ev.Kind()),
			log.FieldSessionID, ev.SessionID(),
			"text_length", len(ev.Text()),
			log.FieldToolName, ev.ToolName())

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
		if sessionID == "" && ev.SessionID() != "" {
			sessionID = ev.SessionID()
			if _, ok := h.Router.Lookup(chatID); ok {
				h.Router.SetSessionID(chatID, sessionID)
			}
		}

		switch ev.Kind() {
		case oc.HighEventPrompt:
			// First event of every turn. Emit SessionInit using the sessionID
			// carried on the prompt marker (SDK's Run pre-fills it from
			// CreateSession/Prompt).
			if sessionID != "" {
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeSessionInit,
					SessionInit: &protocol.SessionInitPayload{
						SessionID: sessionID,
						Model:     resolveModel("", modelSpec),
					},
				})
			}
		case oc.HighEventStepStart:
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
		case oc.HighEventStepFinish:
			// Non-terminal step (finish != "stop"): accumulate its tokens and
			// cost so the usage store gets the full turn total. It produces no
			// card update — the progress card already shows the running step.
			accInput += ev.InputTokens()
			accOutput += ev.OutputTokens()
			accCacheRead += ev.CacheRead()
			accCacheWrite += ev.CacheWrite()
			accCost += ev.Cost()
		case oc.HighEventText:
			text.WriteString(ev.Text())
		case oc.HighEventToolUse:
			h.emitAsync(promptID, &protocol.Control{
				Type:    protocol.TypeToolUse,
				ToolUse: &protocol.ToolUsePayload{Name: ev.ToolName(), Input: bridgebase.SummarizeToolInput(ev.ToolName(), ev.ToolInput())},
			})
		case oc.HighEventToolResult:
			// opencode's "task" tool IS the subagent delegation.
			isSub := ev.ToolName() == "task"
			h.emitAsync(promptID, &protocol.Control{
				Type: protocol.TypeToolResult,
				ToolResult: &protocol.ToolResultPayload{
					Name:       ev.ToolName(),
					Input:      bridgebase.SummarizeToolInput(ev.ToolName(), ev.ToolInput()),
					Output:     ev.Text(),
					IsError:    ev.IsToolError(),
					IsSubagent: isSub,
				},
			})
		case oc.HighEventPermissionAsked:
			// Spawn per asked event: the serve-side agent blocks until the
			// reply, but the event loop must keep draining. The goroutine's
			// ctx is the prompt ctx, so an abort rejects the request.
			if p := ev.PermissionAsked(); p != nil {
				h.Wg.Add(1)
				bridgebase.GoSafe(h.Logger, "permission-asked:"+p.ID, func() {
					defer h.Wg.Done()
					h.handlePermissionAsked(ctx, chatID, promptID, p)
				})
			}
		case oc.HighEventQuestionAsked:
			if q := ev.QuestionAsked(); q != nil {
				h.Wg.Add(1)
				bridgebase.GoSafe(h.Logger, "question-asked:"+q.ID, func() {
					defer h.Wg.Done()
					h.handleQuestionAsked(ctx, chatID, promptID, q)
				})
			}
		case oc.HighEventTodoUpdated:
			// Field-copy SDK Todo → protocol.TodoItem here so the protocol package
			// stays free of any SDK import (no compile cycle). The SDK sends the
			// full list each time; the renderer overwrites, so no merge is needed.
			if tu := ev.TodoUpdated(); tu != nil {
				items := make([]protocol.TodoItem, len(tu.Todos))
				for i, td := range tu.Todos {
					items[i] = protocol.TodoItem{Content: td.Content, Status: td.Status, Priority: td.Priority}
				}
				h.emitAsync(promptID, &protocol.Control{
					Type: protocol.TypeTodo,
					Todo: &protocol.TodoPayload{Todos: items},
				})
			}
		case oc.HighEventResult:
			return h.finalizeResult(ev, text.String(), sessionID, modelSpec, chatID, stepCount, startTime,
				accInput, accOutput, accCacheRead, accCacheWrite, accCost)
		case oc.HighEventError:
			h.Logger.Debug("bridge: error event",
				log.FieldChatID, chatID,
				"error_text", truncateForDebug(ev.Text(), h.debugRedact()))
			return promptResult{
				err:       errors.New(nonEmpty(ev.Text(), "opencode 运行出错")),
				model:     resolveModel("", modelSpec),
				sessionID: sessionID,
			}
		default:
			// Forward-compat: the SDK may emit new HighEventKind values; log
			// at debug so a schema change is observable without breaking the
			// turn.
			h.Logger.Debug("opencode: unhandled event type",
				log.FieldChatID, chatID,
				log.FieldEventType, string(ev.Kind()))
		}
	}

	// Channel closed without a terminal event (defensive; the SDK normally
	// synthesises a HighEventError). If the context was cancelled (user abort
	// or prompt timeout), surface it as a cancellation rather than a generic
	// error so emitTerminal shows the right notice.
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
func (h *Handler) finalizeResult(ev oc.HighEvent, accText, sessionID, modelSpec, chatID string, stepCount int, startTime time.Time,
	accInput, accOutput, accCacheRead, accCacheWrite int, accCost float64) promptResult {
	var durationMs int64
	if !startTime.IsZero() {
		durationMs = time.Since(startTime).Milliseconds()
	}

	// Add the terminal (stop) step's tokens to the accumulated tool-calls
	// steps so the usage breakdown reflects the whole turn.
	totalInput := accInput + ev.InputTokens()
	totalOutput := accOutput + ev.OutputTokens()
	totalCacheRead := accCacheRead + ev.CacheRead()
	totalCacheWrite := accCacheWrite + ev.CacheWrite()

	result := promptResult{
		model:      resolveModel("", modelSpec),
		sessionID:  sessionID,
		durationMs: durationMs,
		// contextTokens stays terminal-step input+output (non-cache) so the
		// result card's token count remains claude-comparable and does not
		// jump when usage accounting started summing every step. The full
		// per-turn breakdown lives in inputTokens/outputTokens/cacheRead/
		// cacheWrite below for the usage store.
		contextTokens: ev.InputTokens() + ev.OutputTokens(),
		costUSD:       accCost + ev.Cost(),
		steps:         stepCount,

		inputTokens:  totalInput,
		outputTokens: totalOutput,
		cacheRead:    totalCacheRead,
		cacheWrite:   totalCacheWrite,
	}

	if ev.IsError() {
		msg := ev.Result()
		if strings.TrimSpace(msg) == "" {
			msg = "opencode 返回错误"
		}
		result.err = errors.New(msg)
		return result
	}

	reply := ev.Result()
	if reply == "" {
		reply = bridgebase.StripThinking(accText, "> ")
	} else {
		reply = bridgebase.StripThinking(reply, "> ")
	}
	h.Logger.Debug("finalize result",
		log.FieldChatID, chatID,
		"result_len", len(ev.Result()),
		"acc_text_len", len(accText),
		"reply_len", len(reply),
		"reply_preview", truncateForDebug(reply, h.debugRedact()))
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
